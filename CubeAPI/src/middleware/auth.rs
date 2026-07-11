// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::{error::AppError, state::AppState};
use axum::{
    extract::{Request, State},
    http::{
        header::{HeaderValue, AUTHORIZATION},
        uri::PathAndQuery,
        Uri,
    },
    middleware::Next,
    response::Response,
};
use std::sync::Arc;
use tokio::sync::RwLock;

/// Auth credential extracted from the request headers.
#[derive(Debug)]
pub(crate) enum AuthCredential {
    /// `Authorization: Bearer <token>`
    Bearer(String),
    /// `X-API-Key: <key>`
    ApiKey(String),
}

/// Propagated auth identity.  Populated by the callback middleware and read by
/// audit / terminal handlers.  When no callback is configured the terminal may
/// fall back to AgentHub session validation.
#[derive(Debug, Clone, Default)]
pub(crate) struct AuthContext {
    /// "bearer" | "api_key" | "session" | "none"
    pub auth_type: String,
    pub user_id: Option<String>,
    pub user_name: Option<String>,
}

pub(crate) type SharedAuthContext = Arc<RwLock<AuthContext>>;

/// Return the existing auth context or create a default one and insert it.
pub(crate) fn get_or_init_auth_context(request: &mut Request) -> SharedAuthContext {
    if let Some(ctx) = request.extensions().get::<SharedAuthContext>().cloned() {
        return ctx;
    }
    let ctx = Arc::new(RwLock::new(AuthContext::default()));
    request.extensions_mut().insert(ctx.clone());
    ctx
}

/// Extract the auth credential from request headers (Bearer takes priority over X-API-Key).
pub(crate) fn extract_credential(request: &Request) -> Option<AuthCredential> {
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
/// Behavior:
/// - If `config.auth_callback_url` is not set (`None`), all requests are allowed through.
/// - If set:
///   1. Extract a Bearer token or X-API-Key from the request headers (Bearer takes priority).
///   2. Forward a POST to the callback URL with:
///      - `Authorization: Bearer <token>`  (when the client used Bearer auth)
///      - `X-API-Key: <key>`              (when the client used API Key auth)
///      - `X-Request-Path: <original path>` (the path the client requested)
///      - `X-Request-Method: <HTTP method>` (the HTTP method the client used, e.g. GET/DELETE)
///   3. HTTP 200 from callback → allow; any other status → 401 Unauthorized.
///
/// The two credential headers are mutually exclusive; the callback receives whichever
/// one the client sent. No extra type discriminator is needed.
///
/// On a successful callback, the response headers `X-User-ID` and `X-User-Name` are
/// captured and propagated to downstream handlers / audit logs.  The callback may omit
/// them if it has no identity to return.
///
/// # Security note
///
/// Multiple HTTP methods (e.g. GET/POST/DELETE/PATCH) are mounted on the same path
/// (e.g. `/templates/:id`). A callback that only whitelists by path cannot distinguish
/// a read from a write or delete operation. Forwarding `X-Request-Method` allows the
/// callback to enforce fine-grained (path + method) authorization.
///
/// For the terminal WebSocket route (`/sandboxes/:sandboxID/terminal/ws`), the
/// callback receives the concrete sandbox ID in `X-Request-Path` and can therefore
/// enforce per-sandbox access control.  This is the recommended authorization model
/// when sandbox ownership data is not otherwise available in CubeAPI.
pub async fn unified_auth(
    State(state): State<AppState>,
    mut request: Request,
    next: Next,
) -> Result<Response, AppError> {
    // No callback configured — pass through immediately.
    let callback_url = match state.config.auth_callback_url.as_deref() {
        Some(url) if !url.is_empty() => url.to_string(),
        _ => return Ok(next.run(request).await),
    };

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

    // Record the credential type for downstream audit logs.
    {
        let auth_type = match &credential {
            AuthCredential::Bearer(_) => "bearer",
            AuthCredential::ApiKey(_) => "api_key",
        };
        let ctx = get_or_init_auth_context(&mut request);
        ctx.write().await.auth_type = auth_type.to_string();
    }

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
        // Capture user identity returned by the callback, if any.
        let user_id = callback_resp
            .headers()
            .get("X-User-ID")
            .and_then(|v| v.to_str().ok())
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty());
        let user_name = callback_resp
            .headers()
            .get("X-User-Name")
            .and_then(|v| v.to_str().ok())
            .map(|v| v.trim().to_string())
            .filter(|v| !v.is_empty());

        {
            let ctx = get_or_init_auth_context(&mut request);
            let mut guard = ctx.write().await;
            guard.user_id = user_id;
            guard.user_name = user_name;
        }

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

/// Rewrite WebSocket auth query parameters into HTTP headers before the
/// unified auth middleware runs.  Browsers cannot set custom headers on a
/// `WebSocket` connection, so the frontend sends credentials as query params:
///   - `?api_key=...`      -> `X-API-Key: ...`
///   - `?access_token=...` -> `Authorization: Bearer ...`
///
/// Existing headers are never overwritten, so clients that can set headers
/// directly continue to work unchanged.
///
/// Also initializes the shared `AuthContext` so that downstream middleware can
/// update it with the authenticated identity.
pub async fn websocket_auth(mut request: Request, next: Next) -> Result<Response, AppError> {
    let query = request.uri().query().unwrap_or("").to_string();

    // Initialise auth context from query params so audit logs know which
    // credential type the browser sent even if auth later fails.
    let mut initial_auth_type = "none";
    for (key, _) in url::form_urlencoded::parse(query.as_bytes()) {
        match key.as_ref() {
            "api_key" => initial_auth_type = "api_key",
            "access_token" if initial_auth_type == "none" => {
                initial_auth_type = "bearer";
            }
            _ => {}
        }
    }
    {
        let ctx = get_or_init_auth_context(&mut request);
        ctx.write().await.auth_type = initial_auth_type.to_string();
    }

    let headers = request.headers_mut();

    for (key, value) in url::form_urlencoded::parse(query.as_bytes()) {
        match key.as_ref() {
            "api_key" if !headers.contains_key("X-API-Key") => {
                let hv = HeaderValue::from_str(&value).map_err(|_| {
                    AppError::BadRequest("invalid api_key query parameter".to_string())
                })?;
                headers.insert("X-API-Key", hv);
            }
            "access_token" if !headers.contains_key(AUTHORIZATION) => {
                let bearer = format!("Bearer {}", value);
                let hv = HeaderValue::from_str(&bearer).map_err(|_| {
                    AppError::BadRequest("invalid access_token query parameter".to_string())
                })?;
                headers.insert(AUTHORIZATION, hv);
            }
            _ => {}
        }
    }

    strip_websocket_auth_query_params(&mut request)?;

    Ok(next.run(request).await)
}

fn strip_websocket_auth_query_params(request: &mut Request) -> Result<(), AppError> {
    let Some(query) = request.uri().query() else {
        return Ok(());
    };

    let mut removed_secret = false;
    let mut serializer = url::form_urlencoded::Serializer::new(String::new());

    for (key, value) in url::form_urlencoded::parse(query.as_bytes()) {
        match key.as_ref() {
            "api_key" | "access_token" => removed_secret = true,
            _ => {
                serializer.append_pair(&key, &value);
            }
        }
    }

    if !removed_secret {
        return Ok(());
    }

    let sanitized_query = serializer.finish();
    let path = request.uri().path();
    let path_and_query = if sanitized_query.is_empty() {
        path.to_string()
    } else {
        format!("{}?{}", path, sanitized_query)
    };

    let mut parts = request.uri().clone().into_parts();
    parts.path_and_query = Some(
        path_and_query
            .parse::<PathAndQuery>()
            .map_err(|_| AppError::BadRequest("invalid WebSocket query parameters".to_string()))?,
    );
    *request.uri_mut() = Uri::from_parts(parts)
        .map_err(|_| AppError::BadRequest("invalid WebSocket URI".to_string()))?;

    Ok(())
}

/// Terminal-only fallback: when no `auth_callback_url` is configured but an
/// AgentHub database is available, treat `?access_token=<session_token>` as a
/// WebUI session token and validate it.  This lets default WebUI deployments
/// carry a user identity in terminal audit logs without requiring a separate
/// auth callback.
///
/// When a callback is configured this middleware is a no-op (the callback is
/// the authoritative source).  When no token is provided the request is allowed
/// through anonymously, preserving backward compatibility for open deployments.
pub async fn terminal_session_auth(
    State(state): State<AppState>,
    mut request: Request,
    next: Next,
) -> Result<Response, AppError> {
    // Callback mode takes precedence.
    if state
        .config
        .auth_callback_url
        .as_deref()
        .is_some_and(|u| !u.is_empty())
    {
        return Ok(next.run(request).await);
    }

    // If unified_auth already ran and produced an identity, do not override.
    {
        let ctx = get_or_init_auth_context(&mut request);
        if ctx.read().await.user_id.is_some() {
            return Ok(next.run(request).await);
        }
    }

    if let Some(AuthCredential::Bearer(token)) = extract_credential(&request) {
        if let Some(store) = &state.agenthub_store {
            let ctx = get_or_init_auth_context(&mut request);
            ctx.write().await.auth_type = "session".to_string();

            match store.validate_session(&token).await {
                Ok(Some(username)) => {
                    ctx.write().await.user_id = Some(username);
                }
                Ok(None) => {
                    return Err(AppError::Unauthorized(
                        "invalid or expired session".to_string(),
                    ));
                }
                Err(e) => {
                    return Err(AppError::Internal(anyhow::anyhow!(
                        "session validation failed: {}",
                        e
                    )));
                }
            }
        }
    }

    Ok(next.run(request).await)
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

    #[tokio::test]
    async fn websocket_auth_strips_credentials_from_request_uri() {
        let mut request = axum::http::Request::builder()
            .uri(
                "/sandboxes/sb-1/terminal/ws?access_token=tok%201&cols=100&api_key=key%202&rows=40&container=web",
            )
            .body(Body::empty())
            .unwrap();

        strip_websocket_auth_query_params(&mut request).unwrap();

        assert_eq!(
            request.uri().to_string(),
            "/sandboxes/sb-1/terminal/ws?cols=100&rows=40&container=web"
        );
    }
}

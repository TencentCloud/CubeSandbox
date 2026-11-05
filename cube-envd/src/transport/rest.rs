// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use futures::FutureExt;
use std::sync::Arc;

use axum::body::{to_bytes, Body};
use axum::extract::State;
use axum::http::{HeaderValue, Request, StatusCode};
use axum::response::{IntoResponse, Response};

use crate::error::DomainError;
use crate::filesystem::upload::UploadError;
use crate::server::LifecycleState;

#[derive(Clone)]
pub struct AppState {
    pub lifecycle: Arc<LifecycleState>,
    pub readiness: Arc<crate::server::ReadinessManager>,
    pub(crate) request_shutdown: Arc<crate::transport::RequestShutdown>,
    pub runtime: Arc<crate::runtime::RuntimeStateStore>,
    pub filesystem: Arc<crate::filesystem::FilesystemService>,
    pub processes: Arc<crate::process::ProcessManager>,
    pub(crate) metrics: Arc<crate::guest::metrics::Metrics>,
    pub users: crate::runtime::UserDatabase,
}

impl AppState {
    pub(crate) fn force_request_shutdown(&self) {
        self.request_shutdown.force();
    }
}

pub async fn health_handler(State(state): State<Arc<AppState>>) -> Response {
    if state.readiness.is_ready().await {
        (
            StatusCode::NO_CONTENT,
            [
                (
                    axum::http::header::CACHE_CONTROL,
                    HeaderValue::from_static("no-store"),
                ),
                (
                    axum::http::header::CONTENT_TYPE,
                    HeaderValue::from_static(""),
                ),
            ],
        )
            .into_response()
    } else {
        error_response(StatusCode::SERVICE_UNAVAILABLE, "unavailable")
    }
}

pub async fn metrics_handler(State(state): State<Arc<AppState>>) -> Response {
    let headers = [
        ("cache-control", "no-store"),
        ("content-type", "application/json"),
    ];
    match state.metrics.sample().await {
        Ok(sample) => (headers, format!("{sample}\n")).into_response(),
        Err(_) => (StatusCode::INTERNAL_SERVER_ERROR, headers).into_response(),
    }
}

pub async fn envs_handler(State(state): State<Arc<AppState>>) -> Response {
    let snapshot = state.runtime.snapshot().await;
    (
        [
            ("cache-control", "no-store"),
            ("content-type", "application/json"),
        ],
        format!(
            "{}\n",
            serde_json::to_string(snapshot.environment()).expect("string map")
        ),
    )
        .into_response()
}

pub async fn init_handler(State(state): State<Arc<AppState>>, request: Request<Body>) -> Response {
    let body = match to_bytes(
        request.into_body(),
        crate::transport::limits::INIT_BODY_LIMIT,
    )
    .await
    {
        Ok(body) => body,
        Err(error) if error.to_string() == "length limit exceeded" => {
            return error_response(StatusCode::PAYLOAD_TOO_LARGE, "request body exceeds limit");
        }
        Err(_) => return error_response(StatusCode::BAD_REQUEST, "malformed request body"),
    };
    let body = zeroize::Zeroizing::new(Vec::<u8>::from(body));
    let body: &[u8] = if body.is_empty() {
        b"{}"
    } else {
        body.as_ref()
    };
    let parsed = match isolate_pure_request(|| crate::init::parse(body)) {
        Ok(parsed) => parsed,
        Err(error) => return error_response(error.http_status(), error.public_message()),
    };
    let init = match parsed {
        Ok(init) => init,
        Err(crate::init::ParseError::TooDeep) => {
            return error_response(StatusCode::BAD_REQUEST, "JSON nesting exceeds limit")
        }
        Err(_) => return StatusCode::BAD_REQUEST.into_response(),
    };
    let has_token = init.access_token.is_some();
    let init_state = state.clone();
    // Waiting requests remain cancellable. Only the serialized side-effect
    // owner survives disconnection, so aborted requests cannot queue detached jobs.
    let effects = state.runtime.updates.clone().lock_owned().await;
    let applied = tokio::spawn(async move {
        match std::panic::AssertUnwindSafe(init_state.runtime.apply_locked(init, effects))
            .catch_unwind()
            .await
        {
            Ok(result) => result,
            Err(_) => {
                init_state
                    .lifecycle
                    .fail_with(crate::server::EnvdFailure::request_panic());
                Err(DomainError::Unavailable)
            }
        }
    })
    .await
    .unwrap_or(Err(DomainError::Unavailable));
    if let Err(error) = applied {
        if error == DomainError::Unauthenticated {
            return (
                StatusCode::UNAUTHORIZED,
                if has_token {
                    "access token validation failed"
                } else {
                    "access token reset not authorized"
                },
            )
                .into_response();
        }
        if error == DomainError::InvalidArgument("invalid timestamp".into()) {
            return StatusCode::BAD_REQUEST.into_response();
        }
        return (error.http_status(), error.public_message().to_owned()).into_response();
    }
    state.runtime.refresh_metadata();
    (
        StatusCode::NO_CONTENT,
        [
            (
                axum::http::header::CACHE_CONTROL,
                HeaderValue::from_static("no-store"),
            ),
            (
                axum::http::header::CONTENT_TYPE,
                HeaderValue::from_static(""),
            ),
        ],
    )
        .into_response()
}

fn isolate_pure_request<T>(
    operation: impl FnOnce() -> T + std::panic::UnwindSafe,
) -> Result<T, DomainError> {
    std::panic::catch_unwind(operation).map_err(|_| DomainError::Internal)
}

pub(crate) fn unavailable() -> Response {
    error_response(StatusCode::SERVICE_UNAVAILABLE, "unavailable")
}

pub async fn file_get_handler(
    State(state): State<Arc<AppState>>,
    request: Request<Body>,
) -> Response {
    let query = match axum::extract::Query::<Vec<(String, String)>>::try_from_uri(request.uri()) {
        Ok(query) => query.0,
        Err(_) => return file_error(StatusCode::BAD_REQUEST, "invalid query"),
    };
    if let Err(response) =
        super::auth::signed_file(&state, request.headers(), request.method(), &query).await
    {
        return response;
    }
    let username = query
        .iter()
        .find(|(key, _)| key == "username")
        .map(|(_, username)| username.clone());
    let path = query
        .iter()
        .find(|(key, _)| key == "path")
        .map(|(_, value)| value.clone())
        .unwrap_or_default();
    let snapshot = state.runtime.snapshot().await;
    match state
        .filesystem
        .download(
            path,
            request.headers().clone(),
            username,
            snapshot,
            state.users.clone(),
        )
        .await
    {
        Ok(response) => response,
        Err(error) => {
            let status = match &error {
                DomainError::ResourceExhausted(message)
                    if message == "filesystem jobs are full" =>
                {
                    StatusCode::TOO_MANY_REQUESTS
                }
                DomainError::ResourceExhausted(_) => StatusCode::SERVICE_UNAVAILABLE,
                _ => error.http_status(),
            };
            file_error(status, error.public_message())
        }
    }
}

pub async fn file_post_handler(
    State(state): State<Arc<AppState>>,
    request: Request<Body>,
) -> Response {
    let query = match axum::extract::Query::<Vec<(String, String)>>::try_from_uri(request.uri()) {
        Ok(query) => query.0,
        Err(_) => return file_error(StatusCode::BAD_REQUEST, "invalid query"),
    };
    if let Err(response) =
        super::auth::signed_file(&state, request.headers(), request.method(), &query).await
    {
        return response;
    }
    let content_encoding = request
        .headers()
        .get("content-encoding")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    let gzip = match super::encoding::coding(content_encoding).0.as_str() {
        "" | "identity" => false,
        "gzip" => true,
        _ => return file_error(StatusCode::BAD_REQUEST,format!("error decompressing request body: unsupported Content-Encoding: {content_encoding}, supported: [gzip]")),
    };
    let content_type = request
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    let boundary = match request
        .headers()
        .get("content-type")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| crate::filesystem::multipart::parameters(value).ok())
    {
        Some((kind, _)) if kind == "application/octet-stream" => Ok(None),
        Some((kind, mut params)) if kind == "multipart/form-data" || kind == "multipart/mixed" => {
            match params.remove("boundary") {
                Some(boundary)
                    if !boundary.is_empty()
                        && boundary.len() <= 70
                        && boundary.bytes().all(|byte| {
                            byte.is_ascii_alphanumeric() || b"'()+_,-./:=? ".contains(&byte)
                        })
                        && !boundary.ends_with(' ') =>
                {
                    Ok(Some(boundary))
                }
                Some(boundary) if !boundary.is_empty() => Err(UploadError::new(StatusCode::BAD_REQUEST,"invalid multipart boundary")),
                _ => Err(UploadError::new(StatusCode::INTERNAL_SERVER_ERROR,"error parsing multipart form: no multipart boundary param in Content-Type")),
            }
        }
        _ => {
            Err(UploadError::new(
                StatusCode::BAD_REQUEST,
                format!("unsupported content type: {content_type}, expected multipart/form-data or application/octet-stream"),
            ))
        }
    };
    let username = query
        .iter()
        .find(|(key, _)| key == "username")
        .map(|(_, username)| username.clone());
    let path = query
        .iter()
        .find(|(key, _)| key == "path")
        .map(|(_, value)| value.clone());
    let snapshot = state.runtime.snapshot().await;
    match state
        .filesystem
        .upload(
            crate::filesystem::upload::Upload {
                path,
                boundary,
                gzip,
                body: request.into_body(),
            },
            username,
            snapshot,
            state.users.clone(),
        )
        .await
    {
        Ok(entries) => (
            [("content-type", "text/plain; charset=utf-8")],
            http_json(&entries),
        )
            .into_response(),
        Err(error) => file_error(error.status, error.message),
    }
}

pub async fn compose_handler(
    State(state): State<Arc<AppState>>,
    request: Request<Body>,
) -> Response {
    let body = match to_bytes(request.into_body(), usize::MAX).await {
        Ok(body) => body,
        Err(_) => return file_error(StatusCode::BAD_REQUEST, "invalid request body"),
    };
    let request = match serde_json::from_slice::<Option<crate::filesystem::compose::Compose>>(&body)
    {
        Ok(request) => request.unwrap_or_default(),
        Err(error) => {
            return file_error(
                StatusCode::BAD_REQUEST,
                format!("invalid request body: {error}"),
            )
        }
    };
    match state
        .filesystem
        .compose(request, state.runtime.snapshot().await, state.users.clone())
        .await
    {
        Ok(entry) => ([("content-type", "application/json")], format!("{entry}\n")).into_response(),
        Err(error) => file_error(error.status, error.message),
    }
}

pub async fn method_not_allowed() -> Response {
    (StatusCode::METHOD_NOT_ALLOWED, [("allow", "POST")]).into_response()
}

fn error_response(status: StatusCode, message: impl Into<String>) -> Response {
    (
        status,
        [
            (
                axum::http::header::CACHE_CONTROL,
                HeaderValue::from_static("no-store"),
            ),
            (
                axum::http::header::CONTENT_TYPE,
                HeaderValue::from_static("text/plain; charset=utf-8"),
            ),
        ],
        message.into(),
    )
        .into_response()
}

pub(crate) fn file_error(status: StatusCode, message: impl Into<String>) -> Response {
    let body =
        http_json(&serde_json::json!({"code":status.as_u16(),"message":message.into()})) + "\n";
    (
        status,
        [
            ("content-type", "application/json; charset=utf-8"),
            ("x-content-type-options", "nosniff"),
        ],
        body,
    )
        .into_response()
}

pub async fn file_method_not_allowed(request: Request<Body>) -> Response {
    let body = if request.method() == http::Method::HEAD {
        Body::from_stream(futures::stream::empty::<
            Result<axum::body::Bytes, std::io::Error>,
        >())
    } else {
        Body::empty()
    };
    Response::builder()
        .status(StatusCode::METHOD_NOT_ALLOWED)
        .header("allow", "GET")
        .header("allow", "POST")
        .body(body)
        .expect("file method response")
}

fn http_json(value: &impl serde::Serialize) -> String {
    serde_json::to_string(value)
        .expect("HTTP JSON")
        .replace('&', "\\u0026")
        .replace('<', "\\u003c")
        .replace('>', "\\u003e")
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn isolated_pure_request_panic_maps_to_internal() {
        let result = isolate_pure_request(|| panic!("isolated panic"));

        assert_eq!(result, Err(crate::error::DomainError::Internal));
    }
}

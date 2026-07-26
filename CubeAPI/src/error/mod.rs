// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::models::ApiError;
use axum::{
    http::{header::RETRY_AFTER, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use thiserror::Error;

#[derive(Debug, Error)]
pub enum AppError {
    #[error("not found: {0}")]
    NotFound(String),

    #[error("unauthorized: {0}")]
    Unauthorized(String),

    #[error("bad request: {0}")]
    #[allow(dead_code)]
    BadRequest(String),

    #[error("internal error: {0}")]
    Internal(#[from] anyhow::Error),

    #[error("conflict: {0}")]
    Conflict(String),

    #[error("service unavailable: {message}")]
    ServiceUnavailable { message: String, retry_after: u64 },

    #[error("too many requests: {0}")]
    TooManyRequests(String),

    #[error("not implemented: {0}")]
    NotImplemented(String),
}

impl IntoResponse for AppError {
    fn into_response(self) -> Response {
        match self {
            AppError::ServiceUnavailable {
                message,
                retry_after,
            } => {
                let mut response = (
                    StatusCode::SERVICE_UNAVAILABLE,
                    Json(ApiError::new(503, message)),
                )
                    .into_response();
                let value = HeaderValue::from_str(&retry_after.to_string())
                    .expect("numeric Retry-After is always a valid header value");
                response.headers_mut().insert(RETRY_AFTER, value);
                response
            }
            AppError::NotFound(msg) => {
                (StatusCode::NOT_FOUND, Json(ApiError::new(404, msg))).into_response()
            }
            AppError::Unauthorized(msg) => {
                (StatusCode::UNAUTHORIZED, Json(ApiError::new(401, msg))).into_response()
            }
            AppError::BadRequest(msg) => {
                (StatusCode::BAD_REQUEST, Json(ApiError::new(400, msg))).into_response()
            }
            AppError::Internal(e) => (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ApiError::new(500, e.to_string())),
            )
                .into_response(),
            AppError::Conflict(msg) => {
                (StatusCode::CONFLICT, Json(ApiError::new(409, msg))).into_response()
            }
            AppError::TooManyRequests(msg) => {
                (StatusCode::TOO_MANY_REQUESTS, Json(ApiError::new(429, msg))).into_response()
            }
            AppError::NotImplemented(msg) => {
                (StatusCode::NOT_IMPLEMENTED, Json(ApiError::new(501, msg))).into_response()
            }
        }
    }
}

pub type AppResult<T> = Result<T, AppError>;

#[cfg(test)]
mod tests {
    use super::AppError;
    use axum::{http::header::RETRY_AFTER, response::IntoResponse};

    #[test]
    fn service_unavailable_includes_retry_after_header() {
        let response = AppError::ServiceUnavailable {
            message: "resume is temporarily unavailable".to_string(),
            retry_after: 5,
        }
        .into_response();

        assert_eq!(
            response.status(),
            axum::http::StatusCode::SERVICE_UNAVAILABLE
        );
        assert_eq!(response.headers().get(RETRY_AFTER).unwrap(), "5");
    }
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::models::ApiError;
use axum::{
    http::StatusCode,
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

    #[error("service unavailable: {0}")]
    ServiceUnavailable(String),

    #[error("too many requests: {0}")]
    TooManyRequests(String),

    #[error("not implemented: {0}")]
    NotImplemented(String),
}

impl IntoResponse for AppError {
    fn into_response(self) -> Response {
        let (status, code, message) = match &self {
            AppError::NotFound(msg) => (StatusCode::NOT_FOUND, 404, msg.clone()),
            AppError::Unauthorized(msg) => (StatusCode::UNAUTHORIZED, 401, msg.clone()),
            AppError::BadRequest(msg) => (StatusCode::BAD_REQUEST, 400, msg.clone()),
            AppError::Internal(e) => (StatusCode::INTERNAL_SERVER_ERROR, 500, e.to_string()),
            AppError::Conflict(msg) => (StatusCode::CONFLICT, 409, msg.clone()),
            AppError::ServiceUnavailable(msg) => {
                (StatusCode::SERVICE_UNAVAILABLE, 503, msg.clone())
            }
            AppError::TooManyRequests(msg) => (StatusCode::TOO_MANY_REQUESTS, 429, msg.clone()),
            AppError::NotImplemented(msg) => (StatusCode::NOT_IMPLEMENTED, 501, msg.clone()),
        };
        (status, Json(ApiError::new(code, message))).into_response()
    }
}

pub type AppResult<T> = Result<T, AppError>;

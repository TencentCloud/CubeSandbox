// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use axum::response::{IntoResponse, Response};
use connectrpc::ErrorCode;
use http::StatusCode;
use thiserror::Error;

#[derive(Debug, Error, Clone, PartialEq, Eq)]
pub enum DomainError {
    #[error("invalid argument: {0}")]
    InvalidArgument(String),
    #[error("unauthenticated")]
    Unauthenticated,
    #[error("permission denied")]
    PermissionDenied,
    #[error("not found: {0}")]
    NotFound(String),
    #[error("conflict: {0}")]
    Conflict(String),
    #[error("resource exhausted: {0}")]
    ResourceExhausted(String),
    #[error("unimplemented: {0}")]
    Unimplemented(String),
    #[error("failed precondition: {0}")]
    FailedPrecondition(String),
    #[error("{0}")]
    UnknownMessage(String),
    #[error(
        "error closing stdin: cannot close stdin for PTY process — send Ctrl+D (0x04) instead"
    )]
    PtyCloseUnsupported,
    #[error("internal error")]
    Internal,
    #[error("{0}")]
    InternalMessage(String),
    #[error("unavailable")]
    Unavailable,
    #[error("cancelled")]
    Cancelled,
    #[error("deadline exceeded")]
    DeadlineExceeded,
}

impl DomainError {
    pub fn http_status(&self) -> StatusCode {
        match self {
            Self::InvalidArgument(_) => StatusCode::BAD_REQUEST,
            Self::Unauthenticated => StatusCode::UNAUTHORIZED,
            Self::PermissionDenied => StatusCode::FORBIDDEN,
            Self::NotFound(_) => StatusCode::NOT_FOUND,
            Self::Conflict(_) => StatusCode::CONFLICT,
            Self::ResourceExhausted(_) => StatusCode::PAYLOAD_TOO_LARGE,
            Self::Unimplemented(_) => StatusCode::NOT_IMPLEMENTED,
            Self::FailedPrecondition(_) => StatusCode::BAD_REQUEST,
            Self::PtyCloseUnsupported
            | Self::UnknownMessage(_)
            | Self::Internal
            | Self::InternalMessage(_) => StatusCode::INTERNAL_SERVER_ERROR,
            Self::Unavailable => StatusCode::SERVICE_UNAVAILABLE,
            Self::Cancelled => StatusCode::REQUEST_TIMEOUT,
            Self::DeadlineExceeded => StatusCode::GATEWAY_TIMEOUT,
        }
    }

    pub fn connect_code(&self) -> ErrorCode {
        match self {
            Self::InvalidArgument(_) => ErrorCode::InvalidArgument,
            Self::Unauthenticated => ErrorCode::Unauthenticated,
            Self::PermissionDenied => ErrorCode::PermissionDenied,
            Self::NotFound(_) => ErrorCode::NotFound,
            Self::Conflict(_) => ErrorCode::AlreadyExists,
            Self::ResourceExhausted(_) => ErrorCode::ResourceExhausted,
            Self::Unimplemented(_) => ErrorCode::Unimplemented,
            Self::FailedPrecondition(_) => ErrorCode::FailedPrecondition,
            Self::PtyCloseUnsupported | Self::UnknownMessage(_) => ErrorCode::Unknown,
            Self::Internal | Self::InternalMessage(_) => ErrorCode::Internal,
            Self::Unavailable => ErrorCode::Unavailable,
            Self::Cancelled => ErrorCode::Canceled,
            Self::DeadlineExceeded => ErrorCode::DeadlineExceeded,
        }
    }

    pub fn public_message(&self) -> &str {
        match self {
            Self::InvalidArgument(msg) => msg,
            Self::Unauthenticated => "unauthenticated",
            Self::PermissionDenied => "permission denied",
            Self::NotFound(msg) => msg,
            Self::Conflict(msg) => msg,
            Self::ResourceExhausted(msg) => msg,
            Self::Unimplemented(msg) => msg,
            Self::FailedPrecondition(msg) => msg,
            Self::UnknownMessage(message) => message,
            Self::PtyCloseUnsupported => "error closing stdin: cannot close stdin for PTY process — send Ctrl+D (0x04) instead",
            Self::Internal => "internal error",
            Self::InternalMessage(message) => message,
            Self::Unavailable => "unavailable",
            Self::Cancelled => "cancelled",
            Self::DeadlineExceeded => "deadline exceeded",
        }
    }
}

impl IntoResponse for DomainError {
    fn into_response(self) -> Response {
        (self.http_status(), self.public_message().to_string()).into_response()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn maps_invalid_argument_to_http_and_connect() {
        let err = DomainError::InvalidArgument("bad".into());
        assert_eq!(err.http_status(), StatusCode::BAD_REQUEST);
        assert_eq!(err.connect_code(), ErrorCode::InvalidArgument);
    }
}

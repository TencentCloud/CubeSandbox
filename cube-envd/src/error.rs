// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Error types for the two protocol surfaces.
//!
//! Baseline-verified split (see README.md "Compatibility scope"):
//! - REST `/files` errors: HTTP status + `{"code":<int>,"message":"..."}`
//! - Connect unary errors: HTTP status mapped from the Connect code +
//!   `{"code":"<connect-code>","message":"..."}`
//! - Connect *streaming* errors: always HTTP 200; the error travels in the
//!   EndStream frame as `{"error":{"code","message"}}`.

use axum::http::StatusCode;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConnectCode {
    InvalidArgument,
    NotFound,
    AlreadyExists,
    PermissionDenied,
    ResourceExhausted,
    Unauthenticated,
    Unimplemented,
    DeadlineExceeded,
    Internal,
}

impl ConnectCode {
    pub fn as_str(self) -> &'static str {
        match self {
            ConnectCode::InvalidArgument => "invalid_argument",
            ConnectCode::NotFound => "not_found",
            ConnectCode::AlreadyExists => "already_exists",
            ConnectCode::PermissionDenied => "permission_denied",
            ConnectCode::ResourceExhausted => "resource_exhausted",
            ConnectCode::Unauthenticated => "unauthenticated",
            ConnectCode::Unimplemented => "unimplemented",
            ConnectCode::DeadlineExceeded => "deadline_exceeded",
            ConnectCode::Internal => "internal",
        }
    }

    /// HTTP status for *unary* error responses, per the Connect spec.
    pub fn http_status(self) -> StatusCode {
        match self {
            ConnectCode::InvalidArgument => StatusCode::BAD_REQUEST,
            ConnectCode::NotFound => StatusCode::NOT_FOUND,
            ConnectCode::AlreadyExists => StatusCode::CONFLICT,
            ConnectCode::PermissionDenied => StatusCode::FORBIDDEN,
            ConnectCode::ResourceExhausted => StatusCode::TOO_MANY_REQUESTS,
            ConnectCode::Unauthenticated => StatusCode::UNAUTHORIZED,
            ConnectCode::Unimplemented => StatusCode::NOT_IMPLEMENTED,
            ConnectCode::DeadlineExceeded => StatusCode::GATEWAY_TIMEOUT,
            ConnectCode::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }
}

#[derive(Debug, Clone)]
pub struct ConnectError {
    pub code: ConnectCode,
    pub message: String,
}

impl ConnectError {
    pub fn new(code: ConnectCode, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }

    pub fn unimplemented(what: &str) -> Self {
        Self::new(
            ConnectCode::Unimplemented,
            format!("{what} is not implemented by cube-envd (see CubeSandbox issue #1227 for the MVP scope)"),
        )
    }

    pub fn body_json(&self) -> String {
        serde_json::json!({
            "code": self.code.as_str(),
            "message": self.message,
        })
        .to_string()
    }

    /// Map an I/O error on `path` to the baseline error vocabulary.
    pub fn from_io(context: &str, path: &str, err: &std::io::Error) -> Self {
        let code = match err.kind() {
            std::io::ErrorKind::NotFound => ConnectCode::NotFound,
            std::io::ErrorKind::PermissionDenied => ConnectCode::PermissionDenied,
            std::io::ErrorKind::AlreadyExists => ConnectCode::AlreadyExists,
            _ if err.raw_os_error() == Some(libc::ENOSPC) => ConnectCode::ResourceExhausted,
            _ => ConnectCode::Internal,
        };
        Self::new(code, format!("{context}: {path}: {err}"))
    }
}

/// REST error body used by `/files`: numeric code mirrors the HTTP status.
#[derive(Debug)]
pub struct RestError {
    pub status: StatusCode,
    pub message: String,
}

impl RestError {
    pub fn new(status: StatusCode, message: impl Into<String>) -> Self {
        Self {
            status,
            message: message.into(),
        }
    }

    pub fn body_json(&self) -> String {
        serde_json::json!({
            "code": self.status.as_u16(),
            "message": self.message,
        })
        .to_string()
    }
}

impl axum::response::IntoResponse for RestError {
    fn into_response(self) -> axum::response::Response {
        let body = self.body_json();
        (
            self.status,
            // Baseline REST errors carry the charset suffix (chi/render).
            [(
                axum::http::header::CONTENT_TYPE,
                "application/json; charset=utf-8",
            )],
            body,
        )
            .into_response()
    }
}

impl axum::response::IntoResponse for ConnectError {
    fn into_response(self) -> axum::response::Response {
        let body = self.body_json();
        (
            self.code.http_status(),
            [(axum::http::header::CONTENT_TYPE, "application/json")],
            body,
        )
            .into_response()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unary_error_body_shape() {
        let e = ConnectError::new(ConnectCode::NotFound, "file not found: x");
        assert_eq!(e.code.http_status(), StatusCode::NOT_FOUND);
        let v: serde_json::Value = serde_json::from_str(&e.body_json()).unwrap();
        assert_eq!(v["code"], "not_found");
        assert_eq!(v["message"], "file not found: x");
    }

    #[test]
    fn rest_error_numeric_code() {
        let e = RestError::new(StatusCode::UNAUTHORIZED, "error looking up user 'x'");
        let v: serde_json::Value = serde_json::from_str(&e.body_json()).unwrap();
        assert_eq!(v["code"], 401);
    }
}

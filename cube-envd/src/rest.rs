// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! REST 面的错误体（与 Connect 面区分）。
//!
//! 参考实现有两条并行的错误通道：
//! - Connect 服务用 connect-go 的错误体（`{"code":"not_found",...}`，code 是字符串枚举）；
//! - REST 处理器（`/files`、`/init`）用 `internal/api/error.go` 的 `jsonError`
//!   （`{"code":404,...}`，code 是**数字 HTTP 状态**，外加
//!   `Content-Type: application/json; charset=utf-8`、`X-Content-Type-Options: nosniff`
//!   与 Go `json.Encoder` 的结尾换行）。
//!
//! 两者都在 SDK 的可观察面上（客户端会按 `code` 分流），因此这里保留双通道，而不是
//! 把 REST 错误也塞进 Connect 错误体。

use axum::{
    http::{header, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
};
use serde::Serialize;

#[derive(Debug, Serialize)]
/// 表示参考实现 `Error` 结构体的 REST 错误体。
struct RestErrorBody<'a> {
    /// 数字 HTTP 状态码。
    code: u16,
    /// 面向调用方的错误说明。
    message: &'a str,
}

#[derive(Debug)]
/// 携带 HTTP 状态与消息的 REST 错误。
pub struct RestError {
    /// 对外的 HTTP 状态码，同时也是错误体里的 `code`。
    status: StatusCode,
    /// 错误说明；文案与参考实现保持一致，便于客户端匹配。
    message: String,
}

/// 提供常用 REST 错误的构造函数。
impl RestError {
    /// 使用指定状态码和消息创建错误。
    pub fn new(status: StatusCode, message: impl Into<String>) -> Self {
        Self {
            status,
            message: message.into(),
        }
    }

    /// 目标路径不存在（参考实现 `download.go:85`）。
    pub fn path_missing(path: &std::path::Path) -> Self {
        Self::new(
            StatusCode::NOT_FOUND,
            format!("path '{}' does not exist", path.display()),
        )
    }

    /// 目标路径是目录（参考实现 `download.go:100`）。
    pub fn path_is_directory(path: &std::path::Path) -> Self {
        Self::new(
            StatusCode::BAD_REQUEST,
            format!("path '{}' is a directory", path.display()),
        )
    }

    /// 请求参数无效。
    pub fn invalid_argument(message: impl Into<String>) -> Self {
        Self::new(StatusCode::BAD_REQUEST, message)
    }

    /// 操作内部失败。
    pub fn internal(message: impl Into<String>) -> Self {
        Self::new(StatusCode::INTERNAL_SERVER_ERROR, message)
    }
}

/// 把 REST 错误序列化为参考实现形状的响应。
impl IntoResponse for RestError {
    /// 输出数字 code、JSON 错误体、Go 风格头部与结尾换行。
    fn into_response(self) -> Response {
        let mut body = serde_json::to_vec(&RestErrorBody {
            code: self.status.as_u16(),
            message: &self.message,
        })
        .unwrap_or_else(|_| br#"{"code":500,"message":"internal error"}"#.to_vec());
        // Go 的 json.Encoder 会追加换行；保持逐字节一致。
        body.push(b'\n');

        (
            self.status,
            [
                (
                    header::CONTENT_TYPE,
                    HeaderValue::from_static("application/json; charset=utf-8"),
                ),
                (
                    header::X_CONTENT_TYPE_OPTIONS,
                    HeaderValue::from_static("nosniff"),
                ),
            ],
            body,
        )
            .into_response()
    }
}

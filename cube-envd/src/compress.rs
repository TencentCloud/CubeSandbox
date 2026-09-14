// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! 响应压缩，行为对齐参考实现。
//!
//! 基线在 Go 侧挂了 gzip 中间件：客户端声明 `Accept-Encoding: gzip` 时压缩
//! **可压缩类型**的响应（`application/json` 会被压缩，连 2 字节的 `{}` 也压），
//! 但流式端点（`application/connect+json`）保持 identity，且不会给没有压缩的
//! 响应补 `Vary: Accept-Encoding`——`/files` 下载是个例外（那里由 Go 的
//! serveContent 路径写入 `Vary: Accept-Encoding`）。
//!
//! 这里刻意不用 `tower_http::compression`：它会给每个响应都加上
//! `Vary: accept-encoding`，而基线的 unary 响应只有 CORS 的 `Vary: Origin`，
//! 那会导致对照套件里大批记录产生无意义的差异。

use std::io::Write;

use axum::{
    body::{to_bytes, Body},
    extract::Request,
    http::{header, HeaderValue, StatusCode},
    middleware::Next,
    response::{IntoResponse, Response},
};
use flate2::{write::GzEncoder, Compression};

/// 只压缩缓冲型 JSON 响应；更大的 body 交给流式路径（本 daemon 的 JSON 都很小）。
const MAX_COMPRESS_BYTES: usize = 8 * 1024 * 1024;

/// 客户端是否接受 gzip。
fn accepts_gzip(request: &Request) -> bool {
    request
        .headers()
        .get(header::ACCEPT_ENCODING)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.to_ascii_lowercase().contains("gzip"))
}

/// 响应是否属于"参考实现会压缩"的类型。
fn is_compressible(response: &Response) -> bool {
    response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .is_some_and(|value| value.starts_with("application/json"))
}

/// 按参考实现的取舍压缩响应。
pub async fn middleware(request: Request, next: Next) -> Response {
    let compress = accepts_gzip(&request);
    let response = next.run(request).await;

    if !compress || response.status() != StatusCode::OK || !is_compressible(&response) {
        return response;
    }

    let (parts, body) = response.into_parts();
    let bytes = match to_bytes(body, MAX_COMPRESS_BYTES).await {
        Ok(bytes) => bytes,
        // 读不出来就原样返回：压缩不是契约的必需项，不能因此让请求失败。
        Err(_) => return Response::from_parts(parts, Body::empty()),
    };

    let mut encoder = GzEncoder::new(Vec::new(), Compression::default());
    if encoder.write_all(&bytes).is_err() {
        return Response::from_parts(parts, Body::from(bytes));
    }
    let compressed = match encoder.finish() {
        Ok(compressed) => compressed,
        Err(_) => return Response::from_parts(parts, Body::from(bytes)),
    };

    let mut response = (parts.status, compressed).into_response();
    let headers = response.headers_mut();
    for (name, value) in parts.headers.iter() {
        if name != header::CONTENT_LENGTH && name != header::CONTENT_ENCODING {
            headers.insert(name, value.clone());
        }
    }
    headers.insert(header::CONTENT_ENCODING, HeaderValue::from_static("gzip"));
    response
}

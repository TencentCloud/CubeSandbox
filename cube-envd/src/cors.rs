// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! 与参考实现等价的 CORS 行为。
//!
//! 上游 Go envd 用 `github.com/rs/cors` 的 permissive 配置包住整个 HTTP server
//! （`e2b-dev/infra@2026.16` `packages/envd/main.go:108-130`）：
//! `AllowedOrigins=["*"]`、`AllowedMethods={HEAD,GET,POST,PUT,PATCH,DELETE}`、
//! `AllowedHeaders=["*"]`、`ExposedHeaders=connectcors.ExposedHeaders()+{Location,
//! Cache-Control, X-Content-Type-Options}`、`MaxAge=2h`。
//!
//! 这里按同样的**可观察行为**实现，而不是直接用 `tower-http` 的 `CorsLayer`：rs/cors
//! 与 tower 层在调用方可见的地方不同——预检**回显**请求方法与请求头而不是列出配置、
//! `Vary` 在 handler 之前写入（handler 自己 Set 过的 `Vary` 会覆盖它）、请求方法不在
//! 白名单时不加 CORS 头而是把请求继续交给 handler（最终得到 405）、预检是独立请求
//! （204，不进入路由）。浏览器直连 SDK 时会看到这些差异，因此它们属于协议面。
//!
//! 参考实现只是把 CORS 当"辅助功能"实现，这里同样不引入配置项：值全部取自上游常量。

use axum::{
    extract::Request,
    http::{header, HeaderMap, HeaderName, HeaderValue, Method, StatusCode},
    middleware::Next,
    response::{IntoResponse, Response},
};

/// `main.go:111-118` 允许的六个方法（OPTIONS 不在此列，它在路由之前就被应答）。
const ALLOWED_METHODS: [&str; 6] = ["HEAD", "GET", "POST", "PUT", "PATCH", "DELETE"];
/// `connectcors.ExposedHeaders()` 的三个 gRPC-Web 头，加上上游追加的三个头，
/// 由 rs/cors 规范化并以 `", "` 连接成一个取值（`cors.go:227-229`）。
const EXPOSED_HEADERS: &str = "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Location, Cache-Control, X-Content-Type-Options";
/// `main.go:36` 的 `maxAge = 2 * time.Hour`。
const MAX_AGE: &str = "7200";
/// rs/cors 未配置私有网络时写入的预检 `Vary`（`cors.go:232-234`）。
const PREFLIGHT_VARY: &str =
    "Origin, Access-Control-Request-Method, Access-Control-Request-Headers";
/// rs/cors 为实际请求写入的 `Vary`（`cors.go:33`）。
const ACTUAL_VARY: &str = "Origin";

/// 判断请求是否为 CORS 预检：`OPTIONS` 且带非空的
/// `Access-Control-Request-Method`（rs/cors `Handler`，`cors.go:283-290`）。
fn is_preflight(method: &Method, request_method: Option<&str>) -> bool {
    *method == Method::OPTIONS && request_method.is_some_and(|value| !value.is_empty())
}

/// rs/cors 对"方法是否允许"的判定：`OPTIONS` 恒为允许（`cors.go:490-492`），
/// 其余方法必须落在配置集合内。
fn is_method_allowed(method: &str) -> bool {
    method == Method::OPTIONS.as_str() || ALLOWED_METHODS.contains(&method)
}

/// 读取请求头中的字符串取值，缺失或非 UTF-8 时视为不存在。
fn read_header(headers: &HeaderMap, name: HeaderName) -> Option<&str> {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .filter(|value| !value.is_empty())
}

/// 写入一个响应头；取值由调用方保证为合法的 header value。
fn set_header(headers: &mut HeaderMap, name: HeaderName, value: &str) {
    if let Ok(value) = HeaderValue::from_str(value) {
        headers.insert(name, value);
    }
}

/// 在路由外层实现上游的 CORS 语义。
///
/// 预检请求在这里直接以 `204` 结束，与 rs/cors 的 `optionsPassthrough=false`
/// 一致；其余请求先交给路由，再按上游的写入顺序补齐 CORS 头。
pub async fn middleware(request: Request, next: Next) -> Response {
    let origin = read_header(request.headers(), header::ORIGIN);
    let requested_method = read_header(request.headers(), header::ACCESS_CONTROL_REQUEST_METHOD);

    if is_preflight(request.method(), requested_method) {
        let mut headers = HeaderMap::new();
        // 上游在判断 origin/方法之前就写入 Vary，因此被拒的预检同样带 Vary
        // （`cors.go:337-344`）。
        set_header(&mut headers, header::VARY, PREFLIGHT_VARY);

        if let Some(requested_method) = requested_method.filter(|value| is_method_allowed(value)) {
            set_header(&mut headers, header::ACCESS_CONTROL_ALLOW_ORIGIN, "*");
            // rs/cors 回显请求方法与请求头，而不是列出配置集合（`cors.go:373-382`）。
            set_header(
                &mut headers,
                header::ACCESS_CONTROL_ALLOW_METHODS,
                requested_method,
            );
            if let Some(requested_headers) =
                read_header(request.headers(), header::ACCESS_CONTROL_REQUEST_HEADERS)
            {
                set_header(
                    &mut headers,
                    header::ACCESS_CONTROL_ALLOW_HEADERS,
                    requested_headers,
                );
            }
            set_header(&mut headers, header::ACCESS_CONTROL_MAX_AGE, MAX_AGE);
        }

        return (StatusCode::NO_CONTENT, headers).into_response();
    }

    // 预检之外，`OPTIONS` 也视为允许（`cors.go:490-492`）。
    let method_allowed = is_method_allowed(request.method().as_str());
    let has_origin = origin.is_some();
    let mut response = next.run(request).await;
    let headers = response.headers_mut();

    // 上游在 handler 之前写入 `Vary: Origin`；handler 若自己 Set 过 `Vary`（例如
    // 上游 `/files` 的 `Vary: Accept-Encoding`）就会整体覆盖它。这里在 handler 之后
    // 补写，只在响应尚无 `Vary` 时写入，得到同一结果。
    if !headers.contains_key(header::VARY) {
        set_header(headers, header::VARY, ACTUAL_VARY);
    }

    if has_origin && method_allowed {
        set_header(headers, header::ACCESS_CONTROL_ALLOW_ORIGIN, "*");
        set_header(
            headers,
            header::ACCESS_CONTROL_EXPOSE_HEADERS,
            EXPOSED_HEADERS,
        );
    }

    response
}

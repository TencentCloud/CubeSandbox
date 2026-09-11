// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use axum::body::Body;
use axum::http::{header, HeaderValue, Method, Request, StatusCode};
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};

pub(super) async fn cors(request: Request<Body>, next: Next) -> Response {
    let origin = request
        .headers()
        .get(header::ORIGIN)
        .is_some_and(|v| !v.is_empty());
    let requested_method = request
        .headers()
        .get(header::ACCESS_CONTROL_REQUEST_METHOD)
        .cloned();
    let preflight = request.method() == Method::OPTIONS
        && requested_method.as_ref().is_some_and(|v| !v.is_empty());
    let allowed =
        |method: &str| matches!(method, "HEAD" | "GET" | "POST" | "PUT" | "PATCH" | "DELETE");
    if preflight {
        let mut response = StatusCode::NO_CONTENT.into_response();
        let headers = response.headers_mut();
        headers.insert(
            header::VARY,
            HeaderValue::from_static(
                "Origin, Access-Control-Request-Method, Access-Control-Request-Headers",
            ),
        );
        if origin
            && requested_method
                .as_ref()
                .and_then(|v| v.to_str().ok())
                .is_some_and(allowed)
        {
            headers.insert(
                header::ACCESS_CONTROL_ALLOW_ORIGIN,
                HeaderValue::from_static("*"),
            );
            headers.insert(
                header::ACCESS_CONTROL_ALLOW_METHODS,
                requested_method.unwrap(),
            );
            if let Some(value) = request
                .headers()
                .get(header::ACCESS_CONTROL_REQUEST_HEADERS)
                .filter(|v| !v.is_empty())
            {
                headers.insert(header::ACCESS_CONTROL_ALLOW_HEADERS, value.clone());
            }
            headers.insert(
                header::ACCESS_CONTROL_MAX_AGE,
                HeaderValue::from_static("7200"),
            );
        }
        return response;
    }
    let allow = origin && allowed(request.method().as_str());
    let mut response = next.run(request).await;
    // Upstream installs CORS first; a route may subsequently replace Vary
    // (successful file downloads set Accept-Encoding). Preserve that replacement.
    if !response.headers().contains_key(header::VARY) {
        response
            .headers_mut()
            .insert(header::VARY, HeaderValue::from_static("Origin"));
    }
    if allow {
        response.headers_mut().insert(
            header::ACCESS_CONTROL_ALLOW_ORIGIN,
            HeaderValue::from_static("*"),
        );
        response.headers_mut().insert(header::ACCESS_CONTROL_EXPOSE_HEADERS, HeaderValue::from_static("Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Location, Cache-Control, X-Content-Type-Options"));
    }
    response
}

// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use super::{framing, grpc, json, limits};
use axum::{
    body::Body,
    middleware::{self, Next},
    response::{IntoResponse, Response},
    Router,
};
use connectrpc::{Dispatcher, Router as ConnectRouter};
use http::Request;

pub(super) fn register(mut router: Router, connect: ConnectRouter) -> Router {
    let methods: Vec<_> = connect
        .methods()
        .map(|path| {
            (
                format!("/{path}"),
                connect.lookup(path).expect("registered method").kind,
            )
        })
        .collect();
    let connect = connect
        .into_axum_service()
        .with_limits(limits::connect_limits())
        .with_compression_policy(connectrpc::CompressionPolicy::default().with_min_size(0))
        .with_interceptor(limits::BudgetErrorInterceptor)
        .with_interceptor(json::ProtoJson);

    for (path, kind) in methods {
        router = router.route(
            &path,
            axum::routing::post_service(connect.clone()).layer(middleware::from_fn(
                move |request, next| rpc_transport(kind, request, next),
            )),
        );
    }
    router
}

const UNARY_ACCEPT_POST: &str = "application/grpc, application/grpc+json, application/grpc+json; charset=utf-8, application/grpc+proto, application/grpc-web, application/grpc-web+json, application/grpc-web+json; charset=utf-8, application/grpc-web+proto, application/json, application/json; charset=utf-8, application/proto";

const STREAM_ACCEPT_POST: &str = "application/connect+json, application/connect+json; charset=utf-8, application/connect+proto, application/grpc, application/grpc+json, application/grpc+json; charset=utf-8, application/grpc+proto, application/grpc-web, application/grpc-web+json, application/grpc-web+json; charset=utf-8, application/grpc-web+proto";

fn rpc_media_supported(kind: connectrpc::router::MethodKind, value: &str) -> bool {
    let mut parts = value.split(';');
    let media = parts.next().unwrap_or("").trim();
    if let Some(parameter) = parts.next() {
        if !media.ends_with("json") || parts.next().is_some() {
            return false;
        }
        let Some((key, value)) = parameter.trim().split_once('=') else {
            return false;
        };
        if key.trim() != "charset" || !matches!(value.trim(), "utf-8" | "\"utf-8\"") {
            return false;
        }
    }
    matches!(
        media,
        "application/grpc"
            | "application/grpc+proto"
            | "application/grpc+json"
            | "application/grpc-web"
            | "application/grpc-web+proto"
            | "application/grpc-web+json"
    ) || if matches!(kind, connectrpc::router::MethodKind::Unary) {
        matches!(media, "application/json" | "application/proto")
    } else {
        matches!(
            media,
            "application/connect+json" | "application/connect+proto"
        )
    }
}

async fn rpc_transport(
    kind: connectrpc::router::MethodKind,
    request: Request<Body>,
    next: Next,
) -> Response {
    if request.method() != http::Method::POST {
        return next.run(request).await;
    }
    let content_type = request
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if !rpc_media_supported(kind, content_type) {
        let accept = if matches!(kind, connectrpc::router::MethodKind::Unary) {
            UNARY_ACCEPT_POST
        } else {
            STREAM_ACCEPT_POST
        };
        return (
            http::StatusCode::UNSUPPORTED_MEDIA_TYPE,
            [("accept-post", accept)],
        )
            .into_response();
    }
    let connect_stream = content_type.starts_with("application/connect+");
    let grpc = content_type.starts_with("application/grpc");
    let grpc_web = content_type.starts_with("application/grpc-web");
    let legacy = request.uri().path().starts_with("/filesystem.Filesystem/")
        && request
            .headers()
            .get("user-agent")
            .is_some_and(|value| value == "connect-python");
    let json_charset =
        content_type.starts_with("application/json;") && content_type.contains("charset");
    let connect_unary = matches!(kind, connectrpc::router::MethodKind::Unary)
        && matches!(
            content_type.split(';').next().unwrap_or("").trim(),
            "application/json" | "application/proto"
        );
    let protocol_version = request
        .headers()
        .get("connect-protocol-version")
        .and_then(|value| value.to_str().ok())
        .unwrap_or("")
        .to_owned();
    let request_compression = request
        .headers()
        .get("content-encoding")
        .and_then(|value| value.to_str().ok())
        .unwrap_or("")
        .to_owned();
    let stream_compression = connectrpc::CompressionRegistry::default().negotiate_encoding(
        request
            .headers()
            .get("connect-accept-encoding")
            .and_then(|value| value.to_str().ok()),
        request
            .headers()
            .get("connect-content-encoding")
            .and_then(|value| value.to_str().ok()),
    ) == Some("gzip");
    let mut response = match framing::request(kind, request).await {
        Ok(request) => next.run(request).await,
        Err(response) => response,
    };
    if connect_unary
        && matches!(
            response.status(),
            http::StatusCode::BAD_REQUEST | http::StatusCode::NOT_IMPLEMENTED
        )
    {
        let (mut parts, body) = response.into_parts();
        let bytes = match axum::body::to_bytes(body, usize::MAX).await {
            Ok(bytes) => bytes,
            Err(_) => return http::StatusCode::INTERNAL_SERVER_ERROR.into_response(),
        };
        let mut replacement = None;
        if let Ok(mut error) = serde_json::from_slice::<serde_json::Value>(&bytes) {
            if let Some(message) = error.get("message").and_then(|value| value.as_str()) {
                let mapped = if message == "unsupported protocol version" {
                    // Go negotiates compression before validating the version.
                    if !matches!(request_compression.as_str(), "" | "identity" | "gzip") {
                        parts.status = http::StatusCode::NOT_IMPLEMENTED;
                        error["code"] = "unimplemented".into();
                        Some(format!(
                            "unknown compression {request_compression:?}: supported encodings are gzip"
                        ))
                    } else {
                        Some(format!(
                            "Connect-Protocol-Version must be \"1\": got {protocol_version:?}"
                        ))
                    }
                } else if matches!(
                    message,
                    "gzip data too short for header" | "gzip header truncated"
                ) {
                    Some("get decompressor: unexpected EOF".into())
                } else {
                    message
                        .strip_prefix("unsupported compression encoding: ")
                        .map(|encoding| {
                            format!(
                                "unknown compression {encoding:?}: supported encodings are gzip"
                            )
                        })
                };
                if let Some(message) = mapped {
                    error["message"] = message.into();
                    replacement = serde_json::to_vec(&error).ok();
                }
            }
        }
        if let Some(bytes) = replacement {
            parts.headers.remove(http::header::CONTENT_LENGTH);
            response = Response::from_parts(parts, Body::from(bytes));
        } else {
            response = Response::from_parts(parts, Body::from(bytes));
        }
    }
    if grpc {
        response = grpc::response(response, grpc_web).await;
    }
    if connect_stream && stream_compression {
        response = framing::compressed_response(response);
    }
    if connect_stream {
        response.headers_mut().insert(
            "connect-accept-encoding",
            http::HeaderValue::from_static("gzip"),
        );
    } else if grpc {
        response.headers_mut().insert(
            "grpc-accept-encoding",
            http::HeaderValue::from_static("gzip"),
        );
    }
    if legacy && !matches!(kind, connectrpc::router::MethodKind::Unary) {
        response
            .headers_mut()
            .insert("x-e2b-legacy-sdk", http::HeaderValue::from_static("true"));
    }
    if connect_unary && response.status() != http::StatusCode::UNSUPPORTED_MEDIA_TYPE {
        response
            .headers_mut()
            .insert("accept-encoding", http::HeaderValue::from_static("gzip"));
        if json_charset && response.status().is_success() {
            response.headers_mut().insert(
                "content-type",
                http::HeaderValue::from_static("application/json; charset=utf-8"),
            );
        }
    }
    if matches!(kind, connectrpc::router::MethodKind::Unary)
        && response.status() == axum::http::StatusCode::UNSUPPORTED_MEDIA_TYPE
    {
        response.headers_mut().insert(
            "accept-post",
            axum::http::HeaderValue::from_static(UNARY_ACCEPT_POST),
        );
    }
    response
}

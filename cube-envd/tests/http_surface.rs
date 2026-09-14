// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! HTTP 层的稳定契约：CORS 响应头、未实现面的错误形状、以及 panic 兜底。
//!
//! 这三件事共同支撑 README 里那句"声明范围之外必须返回稳定、协议正确的错误，
//! 不得 panic、不得静默成功"——因此它们由面向 router 的集成测试钉住。

use axum::{
    body::Body,
    http::{header, Request, StatusCode},
};
use http_body_util::BodyExt;
use tower::ServiceExt;

use cube_envd::app::router;

/// 读取响应头中的字符串取值。
fn header_value(response: &axum::response::Response, name: header::HeaderName) -> Option<String> {
    response
        .headers()
        .get(name)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned)
}

// 验证预检回显请求方法与请求头，并以 204 独立结束。
#[tokio::test]
async fn cors_preflight_echoes_requested_method_and_headers() {
    let response = router()
        .oneshot(
            Request::builder()
                .method("OPTIONS")
                .uri("/process.Process/Start")
                .header(header::ORIGIN, "https://example.test")
                .header(header::ACCESS_CONTROL_REQUEST_METHOD, "POST")
                .header(header::ACCESS_CONTROL_REQUEST_HEADERS, "content-type")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_ALLOW_ORIGIN).as_deref(),
        Some("*")
    );
    // rs/cors 回显请求里声明的方法/请求头，而不是列出配置集合。
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_ALLOW_METHODS).as_deref(),
        Some("POST")
    );
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_ALLOW_HEADERS).as_deref(),
        Some("content-type")
    );
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_MAX_AGE).as_deref(),
        Some("7200")
    );
    assert_eq!(
        header_value(&response, header::VARY).as_deref(),
        Some("Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
    );
}

// 验证白名单之外的方法在预检里仍以 204 结束，但不带任何 CORS 许可头。
#[tokio::test]
async fn cors_preflight_for_disallowed_method_omits_allow_headers() {
    let response = router()
        .oneshot(
            Request::builder()
                .method("OPTIONS")
                .uri("/process.Process/Start")
                .header(header::ORIGIN, "https://example.test")
                .header(header::ACCESS_CONTROL_REQUEST_METHOD, "TRACE")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_ALLOW_ORIGIN),
        None
    );
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_ALLOW_METHODS),
        None
    );
    // 上游在判断 origin/方法之前就写入 Vary，被拒的预检同样保留它。
    assert!(header_value(&response, header::VARY).is_some());
}

// 验证带 Origin 的实际请求拿到许可头与暴露头。
#[tokio::test]
async fn cors_actual_request_gets_origin_and_exposed_headers() {
    let response = router()
        .oneshot(
            Request::builder()
                .uri("/health")
                .header(header::ORIGIN, "https://example.test")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_ALLOW_ORIGIN).as_deref(),
        Some("*")
    );
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_EXPOSE_HEADERS).as_deref(),
        Some("Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Location, Cache-Control, X-Content-Type-Options")
    );
    assert_eq!(
        header_value(&response, header::VARY).as_deref(),
        Some("Origin")
    );
}

// 验证不带 Origin 的请求只写入 Vary，不写入许可头。
#[tokio::test]
async fn cors_request_without_origin_only_varies() {
    let response = router()
        .oneshot(
            Request::builder()
                .uri("/health")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(
        header_value(&response, header::VARY).as_deref(),
        Some("Origin")
    );
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_ALLOW_ORIGIN),
        None
    );
    assert_eq!(
        header_value(&response, header::ACCESS_CONTROL_EXPOSE_HEADERS),
        None
    );
}

// 验证声明范围之外的 REST 面返回 501 + Connect 错误体，而不是 404。
#[tokio::test]
async fn unimplemented_rest_surfaces_answer_501_with_a_connect_error_body() {
    for (method, path) in [("POST", "/files/compose"), ("GET", "/metrics")] {
        let response = router()
            .oneshot(
                Request::builder()
                    .method(method)
                    .uri(path)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(
            response.status(),
            StatusCode::NOT_IMPLEMENTED,
            "{method} {path} must answer 501"
        );
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let json: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(json["code"], "unimplemented", "{method} {path}: {json}");
        assert!(
            json["message"].as_str().is_some_and(|m| !m.is_empty()),
            "{method} {path} must carry a message"
        );
    }
}

// 验证未知路径不会被误当成"已实现"，而是明确的 404。
#[tokio::test]
async fn unknown_paths_answer_404() {
    let response = router()
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/process.Process/Nonexistent")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::NOT_FOUND);
}

// 验证 panic 兜底函数返回稳定的 500 + Connect 错误体，而不是让连接中断。
#[test]
fn panic_response_is_a_stable_internal_error() {
    let response = cube_envd::app::panic_response(Box::new("boom"));

    assert_eq!(response.status(), StatusCode::INTERNAL_SERVER_ERROR);
    assert_eq!(
        response
            .headers()
            .get(header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok()),
        Some("application/json")
    );
}

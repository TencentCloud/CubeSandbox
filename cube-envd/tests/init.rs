// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
};
use cube_envd::app::router;
use http_body_util::BodyExt;
use tower::ServiceExt;

// 验证 init 请求会整体替换环境变量快照。
#[tokio::test]
async fn init_merges_into_the_environment_snapshot() {
    let app = router();

    let response = app
        .clone()
        .oneshot(
            Request::post("/init")
                .header(CONTENT_TYPE, "application/json")
                .body(Body::from(r#"{"envVars":{"LANG":"C","PATH":"/bin"}}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::NO_CONTENT);

    let response = app
        .clone()
        .oneshot(
            Request::post("/init")
                .header(CONTENT_TYPE, "application/json")
                .body(Body::from(r#"{"envVars":{"LANG":"en_US.UTF-8"}}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::NO_CONTENT);

    let response = app
        .oneshot(Request::get("/envs").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let body = response.into_body().collect().await.unwrap().to_bytes();
    // 参考实现逐键 Store（init.go:189-196），因此第一次 /init 的 PATH 仍在，
    // 且启动时种入的 E2B_SANDBOX 不会被后续 /init 抹掉。
    assert_eq!(
        body.as_ref(),
        b"{\"E2B_SANDBOX\":\"false\",\"LANG\":\"en_US.UTF-8\",\"PATH\":\"/bin\"}\n"
    );
}

// 验证 init 请求拒绝未声明字段。
#[tokio::test]
async fn init_rejects_unknown_fields() {
    let response = router()
        .oneshot(
            Request::post("/init")
                .header(CONTENT_TYPE, "application/json")
                .body(Body::from(r#"{"accessToken":"not-supported"}"#))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
}

// 验证超过一元请求体上限的 init 请求不会改动现有环境变量。
#[tokio::test]
async fn init_rejects_bodies_larger_than_the_unary_limit_without_replacing_environment() {
    let app = router();
    let response = app
        .clone()
        .oneshot(
            Request::post("/init")
                .header(CONTENT_TYPE, "application/json")
                .body(Body::from(r#"{"envVars":{"PRESERVED":"yes"}}"#))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::NO_CONTENT);

    // 上限与 SDK 的 Connect 载荷上限同源（见 cube_envd::connect），这里从常量派生，
    // 上限调整时本用例自动跟随。
    let oversized_value = "x".repeat(cube_envd::connect::MAX_UNARY_JSON_BYTES);
    let oversized_body = format!(r#"{{"envVars":{{"TOO_LARGE":"{oversized_value}"}}}}"#);
    let response = app
        .clone()
        .oneshot(
            Request::post("/init")
                .header(CONTENT_TYPE, "application/json")
                .body(Body::from(oversized_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::PAYLOAD_TOO_LARGE);

    let response = app
        .oneshot(Request::get("/envs").body(Body::empty()).unwrap())
        .await
        .unwrap();
    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(
        body.as_ref(),
        b"{\"E2B_SANDBOX\":\"false\",\"PRESERVED\":\"yes\"}\n"
    );
}

use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
};
use cube_envd::app::router;
use http_body_util::BodyExt;
use tower::ServiceExt;

// 验证 init 请求会整体替换环境变量快照。
#[tokio::test]
async fn init_replaces_the_environment_snapshot() {
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
    assert_eq!(body.as_ref(), br#"{"LANG":"en_US.UTF-8"}"#);
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

// 验证超过一 MiB 的 init 请求不会替换现有环境变量。
#[tokio::test]
async fn init_rejects_bodies_larger_than_one_mebibyte_without_replacing_environment() {
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

    let oversized_value = "x".repeat(1024 * 1024);
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
    assert_eq!(body.as_ref(), br#"{"PRESERVED":"yes"}"#);
}

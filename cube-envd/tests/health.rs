use axum::{
    body::{Body, HttpBody},
    http::{Request, StatusCode},
};
use cube_envd::app::router;
use tower::ServiceExt;

// 验证健康检查返回无内容状态和结束响应体。
#[tokio::test]
async fn health_returns_no_content() {
    let response = router()
        .oneshot(Request::get("/health").body(Body::empty()).unwrap())
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    assert!(response.into_body().is_end_stream());
}

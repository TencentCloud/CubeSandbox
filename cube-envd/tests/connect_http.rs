use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
};
use cube_envd::{app::router, connect::END_STREAM_FLAG};
use http_body_util::BodyExt;
use tower::ServiceExt;

// 验证一元 Connect 路由缺少协议版本头时返回参数错误。
#[tokio::test]
async fn unary_connect_routes_require_the_protocol_headers() {
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/Stat")
                .header(CONTENT_TYPE, "application/json")
                .body(Body::from(r#"{"path":"/tmp"}"#))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&body).unwrap()["code"],
        "invalid_argument"
    );
}

// 验证未实现的持久 watcher RPC 返回 Connect 未实现错误。
#[tokio::test]
async fn unimplemented_watcher_api_returns_a_connect_error() {
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/CreateWatcher")
                .header(CONTENT_TYPE, "application/json")
                .header("Connect-Protocol-Version", "1")
                .body(Body::from("{}"))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::NOT_IMPLEMENTED);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&body).unwrap()["code"],
        "unimplemented"
    );
}

// 验证流结束标志不会与普通消息帧标志混淆。
#[test]
fn connect_end_stream_flag_stays_distinct_from_message_data() {
    assert_ne!(END_STREAM_FLAG, 0);
}

// 验证 Connect RPC 的媒体类型不匹配返回 415 和 invalid_argument 错误体。
#[tokio::test]
async fn connect_routes_reject_an_invalid_content_type_with_unsupported_media_type() {
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/Stat")
                .header(CONTENT_TYPE, "text/plain")
                .header("Connect-Protocol-Version", "1")
                .body(Body::from(r#"{"path":"/tmp"}"#))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::UNSUPPORTED_MEDIA_TYPE);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&body).unwrap()["code"],
        "invalid_argument"
    );
}

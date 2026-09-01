use std::fs;

use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
};
use cube_envd::app::router;
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tempfile::tempdir;
use tower::ServiceExt;

mod common;

// 发送一元文件系统 RPC 并解码其状态码和 JSON 响应体。
async fn rpc(app: axum::Router, method: &str, payload: Value) -> (StatusCode, Value) {
    let response = app
        .oneshot(
            Request::post(format!("/filesystem.Filesystem/{method}"))
                .header(CONTENT_TYPE, "application/json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(payload.to_string()))
                .unwrap(),
        )
        .await
        .unwrap();
    let status = response.status();
    let body = response.into_body().collect().await.unwrap().to_bytes();
    let json = if body.is_empty() {
        json!({})
    } else {
        serde_json::from_slice(&body).unwrap()
    };
    (status, json)
}

// 验证文件系统 RPC 能串联完成 stat、列表、移动、建目录和删除。
#[tokio::test]
async fn filesystem_rpc_creates_lists_moves_stats_and_removes_entries() {
    let directory = tempdir().unwrap();
    let source = directory.path().join("nested/source.txt");
    let destination = directory.path().join("nested/moved.txt");
    let app = router();
    fs::create_dir_all(source.parent().unwrap()).unwrap();
    fs::write(&source, b"cube-envd").unwrap();

    let (status, body) = rpc(app.clone(), "Stat", json!({"path": source})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["type"], "FILE_TYPE_FILE");
    assert_eq!(body["entry"]["size"], "9");

    let (status, body) = rpc(
        app.clone(),
        "ListDir",
        json!({"path": directory.path(), "depth": 2}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert!(body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .any(|entry| entry["name"] == "source.txt"));

    let (status, body) = rpc(
        app.clone(),
        "Move",
        json!({"source": source, "destination": destination}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["name"], "moved.txt");
    assert!(destination.exists());

    let new_dir = directory.path().join("another/deep/dir");
    let (status, body) = rpc(app.clone(), "MakeDir", json!({"path": new_dir})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["type"], "FILE_TYPE_DIRECTORY");

    let (status, body) = rpc(app, "Remove", json!({"path": directory.path()})).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body, json!({}));
    assert!(!directory.path().exists());
}

// 验证文件系统 RPC 会拒绝未知 Basic 用户。
#[tokio::test]
async fn filesystem_rpc_rejects_unknown_basic_users() {
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/Stat")
                .header(CONTENT_TYPE, "application/json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", "Basic bm90LWEtdXNlcjo=")
                .body(Body::from(r#"{"path":"/tmp"}"#))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
}

// 验证测试环境提供了用于文件系统 RPC 的用户名。
#[test]
fn test_user_name_is_available_for_filesystem_requests() {
    assert!(!common::current_username().is_empty());
}

// 验证 Filesystem.Stat 返回符号链接类型及其目标路径。
#[cfg(unix)]
#[tokio::test]
async fn filesystem_stat_reports_symbolic_link_metadata() {
    let directory = tempdir().unwrap();
    let target = directory.path().join("target.txt");
    let link = directory.path().join("link.txt");
    fs::write(&target, b"target").unwrap();
    std::os::unix::fs::symlink(&target, &link).unwrap();

    let (status, body) = rpc(router(), "Stat", json!({"path": link})).await;

    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["entry"]["type"], "FILE_TYPE_SYMLINK");
    assert_eq!(body["entry"]["symlinkTarget"], target.display().to_string());
}

use std::fs;

use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
};
use cube_envd::app::router;
use http_body_util::BodyExt;
use tempfile::tempdir;
use tower::ServiceExt;

mod common;

// 验证原始上传按查询用户写入，并能在不带 Basic 认证时读回。
#[tokio::test]
async fn raw_file_upload_is_read_back_without_using_basic_auth() {
    let user = common::current_username();
    let directory = tempdir().unwrap();
    let path = directory.path().join("nested/example.txt");
    let target = format!("/files?path={}&username={user}", path.display());
    let app = router();

    let response = app
        .clone()
        .oneshot(
            Request::post(&target)
                .header(CONTENT_TYPE, "application/octet-stream")
                .header("Authorization", "Basic bm90LWEtcmVhbC11c2VyOg==")
                .body(Body::from("first version"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let response = app
        .oneshot(Request::get(&target).body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .as_ref(),
        b"first version"
    );
    assert_eq!(fs::read(path).unwrap(), b"first version");
}

// 验证上传必须提供路径，且下载目录会被拒绝。
#[tokio::test]
async fn raw_upload_requires_path_and_rejects_directory_reads() {
    let user = common::current_username();
    let directory = tempdir().unwrap();
    let app = router();

    let response = app
        .clone()
        .oneshot(
            Request::post(format!("/files?username={user}"))
                .header(CONTENT_TYPE, "application/octet-stream")
                .body(Body::from("data"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);

    let response = app
        .oneshot(
            Request::get(format!(
                "/files?path={}&username={user}",
                directory.path().display()
            ))
            .body(Body::empty())
            .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
}

// 验证流式原始上传会用新内容原子替换已有目标文件。
#[tokio::test]
async fn raw_upload_replaces_the_target_atomically_after_streaming() {
    let user = common::current_username();
    let directory = tempdir().unwrap();
    let path = directory.path().join("old.txt");
    fs::write(&path, "old content").unwrap();
    let target = format!("/files?path={}&username={user}", path.display());

    let response = router()
        .oneshot(
            Request::post(target)
                .header(CONTENT_TYPE, "application/octet-stream")
                .body(Body::from("new content"))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(fs::read(path).unwrap(), b"new content");
}

// 验证 multipart 上传在缺少查询路径时使用每个字段的文件名。
#[tokio::test]
async fn multipart_upload_uses_each_part_filename_when_path_is_absent() {
    let user = common::current_username();
    let directory = tempdir().unwrap();
    let boundary = "cube-envd-boundary";
    let body = format!(
        "--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{}/from-form.txt\"\r\nContent-Type: application/octet-stream\r\n\r\nfrom multipart\r\n--{boundary}--\r\n",
        directory.path().display()
    );

    let response = router()
        .oneshot(
            Request::post(format!("/files?username={user}"))
                .header(
                    CONTENT_TYPE,
                    format!("multipart/form-data; boundary={boundary}"),
                )
                .body(Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::OK);
    assert_eq!(
        fs::read(directory.path().join("from-form.txt")).unwrap(),
        b"from multipart"
    );
}

// 验证原始上传下载会逐字节保留二进制文件内容。
#[tokio::test]
async fn raw_file_transfer_preserves_binary_bytes() {
    let user = common::current_username();
    let directory = tempdir().unwrap();
    let path = directory.path().join("binary.bin");
    let payload = vec![0, 1, 2, 0x7f, 0x80, 0xff];
    let target = format!("/files?path={}&username={user}", path.display());
    let app = router();

    let response = app
        .clone()
        .oneshot(
            Request::post(&target)
                .header(CONTENT_TYPE, "application/octet-stream")
                .body(Body::from(payload.clone()))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let response = app
        .oneshot(Request::get(&target).body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .as_ref(),
        payload.as_slice()
    );
}

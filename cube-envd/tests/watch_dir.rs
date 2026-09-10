mod common;

use std::fs;

use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
};
use cube_envd::{
    app::router,
    connect::{decode_frame, encode_frame},
};
use futures_util::StreamExt;
use serde_json::json;
use tempfile::tempdir;
use tower::ServiceExt;

// 验证 WatchDir 流先发送 start 事件，再发送文件创建或写入事件。
#[tokio::test]
async fn watch_dir_streams_start_and_file_events() {
    let directory = tempdir().unwrap();
    let request_body =
        encode_frame(0, json!({"path": directory.path()}).to_string().as_bytes()).unwrap();
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/WatchDir")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(request_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let mut stream = response.into_body().into_data_stream();
    let first = stream.next().await.unwrap().unwrap();
    let first = decode_frame(&first).unwrap();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&first.payload).unwrap(),
        json!({"start": {}})
    );

    fs::write(directory.path().join("new.txt"), "watched").unwrap();
    let event = tokio::time::timeout(std::time::Duration::from_secs(3), async {
        loop {
            let bytes = stream.next().await.unwrap().unwrap();
            let frame = decode_frame(&bytes).unwrap();
            let payload: serde_json::Value = serde_json::from_slice(&frame.payload).unwrap();
            if payload.get("filesystem").is_some() {
                return payload;
            }
        }
    })
    .await
    .expect("watch event before timeout");

    assert_eq!(event["filesystem"]["name"], "new.txt");
    assert!(matches!(
        event["filesystem"]["type"].as_str(),
        Some("EVENT_TYPE_CREATE") | Some("EVENT_TYPE_WRITE")
    ));
}

// 验证空闲 WatchDir 流会按客户端请求发送保活事件。
#[tokio::test]
async fn watch_dir_sends_keepalives_during_idle_periods() {
    let directory = tempdir().unwrap();
    let request_body =
        encode_frame(0, json!({"path": directory.path()}).to_string().as_bytes()).unwrap();
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/WatchDir")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Keepalive-Ping-Interval", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(request_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let mut stream = response.into_body().into_data_stream();
    let start = decode_frame(&stream.next().await.unwrap().unwrap()).unwrap();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&start.payload).unwrap(),
        json!({"start": {}})
    );

    let keepalive = tokio::time::timeout(std::time::Duration::from_secs(2), stream.next())
        .await
        .expect("idle WatchDir keepalive before timeout")
        .unwrap()
        .unwrap();
    let keepalive = decode_frame(&keepalive).unwrap();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&keepalive.payload).unwrap(),
        json!({"keepalive": {}})
    );
}

// 验证 chmod 上报为 CHMOD（此前因映射分支不可达而被误报为 WRITE）。
#[cfg(unix)]
#[tokio::test]
async fn watch_dir_reports_chmod_events() {
    use std::os::unix::fs::PermissionsExt;

    let directory = tempdir().unwrap();
    let file = directory.path().join("mode.txt");
    fs::write(&file, "watched").unwrap();

    let mut stream = open_watch(directory.path()).await;

    fs::set_permissions(&file, fs::Permissions::from_mode(0o600)).unwrap();
    let kind = wait_for_event(&mut stream, "mode.txt").await;

    assert_eq!(
        kind, "EVENT_TYPE_CHMOD",
        "chmod must not be reported as another event type"
    );
}

// 验证目录内改名产生上游语义的两条事件：源端 RENAME、目标端 CREATE。
#[cfg(unix)]
#[tokio::test]
async fn watch_dir_reports_rename_as_rename_and_create() {
    let directory = tempdir().unwrap();
    let source = directory.path().join("before.txt");
    fs::write(&source, "watched").unwrap();

    let mut stream = open_watch(directory.path()).await;

    let destination = directory.path().join("after.txt");
    fs::rename(&source, &destination).unwrap();

    let mut seen = Vec::new();
    for _ in 0..2 {
        let event = next_filesystem_event(&mut stream).await;
        seen.push((
            event["name"].as_str().unwrap_or_default().to_owned(),
            event["type"].as_str().unwrap_or_default().to_owned(),
        ));
    }

    assert!(
        seen.contains(&("before.txt".to_owned(), "EVENT_TYPE_RENAME".to_owned())),
        "rename source event missing: {seen:?}"
    );
    assert!(
        seen.contains(&("after.txt".to_owned(), "EVENT_TYPE_CREATE".to_owned())),
        "rename destination must be reported as CREATE: {seen:?}"
    );
}

// 打开一个 WatchDir 流并消费掉 start 帧。
#[cfg(unix)]
async fn open_watch(
    path: &std::path::Path,
) -> impl futures_util::Stream<Item = Result<bytes::Bytes, axum::Error>> + Unpin {
    let request_body = encode_frame(0, json!({"path": path}).to_string().as_bytes()).unwrap();
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/WatchDir")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(request_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let mut stream = response.into_body().into_data_stream();
    let start = decode_frame(&stream.next().await.unwrap().unwrap()).unwrap();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&start.payload).unwrap(),
        json!({"start": {}})
    );
    stream
}

// 等待指定名称的文件系统事件并返回其协议类型。
#[cfg(unix)]
async fn wait_for_event(
    stream: &mut (impl futures_util::Stream<Item = Result<bytes::Bytes, axum::Error>> + Unpin),
    name: &str,
) -> String {
    tokio::time::timeout(std::time::Duration::from_secs(5), async {
        loop {
            let event = next_filesystem_event(stream).await;
            if event["name"] == name {
                return event["type"].as_str().unwrap_or_default().to_owned();
            }
        }
    })
    .await
    .expect("watch event before timeout")
}

// 读取下一个文件系统事件，跳过保活帧。
#[cfg(unix)]
async fn next_filesystem_event(
    stream: &mut (impl futures_util::Stream<Item = Result<bytes::Bytes, axum::Error>> + Unpin),
) -> serde_json::Value {
    loop {
        let bytes = stream.next().await.unwrap().unwrap();
        let frame = decode_frame(&bytes).unwrap();
        let payload: serde_json::Value = serde_json::from_slice(&frame.payload).unwrap();
        if let Some(filesystem) = payload.get("filesystem") {
            return filesystem.clone();
        }
    }
}

// 验证压缩请求帧会在创建 watcher 前被拒绝。
#[tokio::test]
async fn watch_dir_rejects_compressed_requests_before_creating_a_watcher() {
    let directory = tempdir().unwrap();
    let request_body = encode_frame(
        0x01,
        json!({"path": directory.path()}).to_string().as_bytes(),
    )
    .unwrap();
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/WatchDir")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(request_body))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::NOT_IMPLEMENTED);
}

// 验证慢订阅者导致 watcher 队列溢出时会收到资源耗尽结束帧。
#[tokio::test]
async fn watch_dir_ends_a_slow_subscriber_with_resource_exhausted() {
    let directory = tempdir().unwrap();
    let request_body =
        encode_frame(0, json!({"path": directory.path()}).to_string().as_bytes()).unwrap();
    let response = router()
        .oneshot(
            Request::post("/filesystem.Filesystem/WatchDir")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(request_body))
                .unwrap(),
        )
        .await
        .unwrap();
    let mut stream = response.into_body().into_data_stream();
    let _ = stream.next().await.unwrap().unwrap();

    for index in 0..300 {
        fs::write(directory.path().join(format!("{index}.txt")), "watched").unwrap();
    }

    let end = tokio::time::timeout(std::time::Duration::from_secs(5), async {
        loop {
            let bytes = stream.next().await.unwrap().unwrap();
            let frame = decode_frame(&bytes).unwrap();
            if frame.flags == cube_envd::connect::END_STREAM_FLAG {
                return serde_json::from_slice::<serde_json::Value>(&frame.payload).unwrap();
            }
        }
    })
    .await
    .expect("slow subscriber end stream");

    assert_eq!(end["error"]["code"], "resource_exhausted");
}

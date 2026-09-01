use std::{sync::Arc, time::Duration};

use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
};
use cube_envd::{
    app::router,
    connect::{decode_frame, encode_frame, END_STREAM_FLAG},
};
use futures_util::StreamExt;
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

mod common;

// 验证按标签重连时可获取已结束进程的输入和结束事件。
#[tokio::test]
async fn process_list_input_and_terminal_connect_work_by_tag() {
    let app = router();
    let start = stream_request(
        "Start",
        json!({
            "process": {"cmd":"/bin/sh", "args":["-c", "read line; printf '%s' \"$line\""], "envs": {}},
            "tag": "input-test",
            "stdin": true
        }),
    );
    let response = app.clone().oneshot(start).await.unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let mut start_stream = response.into_body().into_data_stream();
    let pid = start_pid(start_stream.next().await.unwrap().unwrap()).unwrap();
    drop(start_stream);

    let (status, body) = unary(app.clone(), "List", json!({})).await;
    assert_eq!(status, StatusCode::OK);
    assert!(body["processes"]
        .as_array()
        .unwrap()
        .iter()
        .any(|process| process["pid"] == pid));

    let (status, _) = unary(
        app.clone(),
        "SendInput",
        json!({"process":{"pid":pid},"input":{"stdin":"aGVsbG8K"}}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    tokio::time::sleep(Duration::from_millis(50)).await;
    let response = app
        .oneshot(stream_request(
            "Connect",
            json!({"process":{"tag":"input-test"}}),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let frames = all_frames(response.into_body().collect().await.unwrap().to_bytes());
    assert!(frames
        .iter()
        .any(|frame| frame["event"]["end"]["exited"] == true));
    assert_eq!(frames.last(), Some(&json!({"end": {}})));
}

// 验证信号可终止存活进程，未知 PID 返回未找到错误。
#[tokio::test]
async fn signal_terminates_a_live_process_and_unknown_pid_is_not_found() {
    let app = router();
    let response = app
        .clone()
        .oneshot(stream_request(
            "Start",
            json!({
                "process": {"cmd":"/bin/sh", "args":["-c", "sleep 30"], "envs": {}},
                "tag": "signal-test",
                "stdin": false
            }),
        ))
        .await
        .unwrap();
    let mut stream = response.into_body().into_data_stream();
    let pid = start_pid(stream.next().await.unwrap().unwrap()).unwrap();
    drop(stream);

    let (status, _) = unary(
        app.clone(),
        "SendSignal",
        json!({"process":{"pid":pid},"signal":"SIGNAL_SIGTERM"}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    tokio::time::sleep(Duration::from_millis(50)).await;
    let (status, body) = unary(app.clone(), "CloseStdin", json!({"process":{"pid":999999}})).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    assert_eq!(body["code"], "not_found");

    let response = app
        .oneshot(stream_request(
            "Connect",
            json!({"process":{"tag":"signal-test"}}),
        ))
        .await
        .unwrap();
    let frames = all_frames(response.into_body().collect().await.unwrap().to_bytes());
    let end = frames
        .iter()
        .find_map(|frame| frame["event"].get("end"))
        .expect("signal-terminated process must have an EndEvent");
    assert_eq!(end["exitCode"], 143);
    assert!(
        end.get("exited").is_none(),
        "proto JSON omits the false exited field: {end}"
    );
    assert_eq!(end["error"], "terminated by signal 15");
}

// 验证 StreamInput 按 start、data 帧顺序写入进程 stdin。
#[tokio::test]
async fn stream_input_consumes_an_ordered_connect_client_stream() {
    let app = router();
    let response = app
        .clone()
        .oneshot(stream_request("Start", json!({
            "process": {"cmd":"/bin/sh", "args":["-c", "read value; printf '%s' \"$value\""], "envs": {}},
            "tag": "stream-input-test",
            "stdin": true
        })))
        .await
        .unwrap();
    let mut stream = response.into_body().into_data_stream();
    let pid = start_pid(stream.next().await.unwrap().unwrap()).unwrap();
    drop(stream);

    let mut body = encode_frame(
        0,
        json!({"start":{"process":{"pid":pid}}})
            .to_string()
            .as_bytes(),
    )
    .unwrap();
    body.extend_from_slice(
        &encode_frame(
            0,
            json!({"data":{"input":{"stdin":"c3RyZWFtZWQK"}}})
                .to_string()
                .as_bytes(),
        )
        .unwrap(),
    );
    let response = app
        .clone()
        .oneshot(
            Request::post("/process.Process/StreamInput")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    tokio::time::sleep(Duration::from_millis(50)).await;
    let response = app
        .oneshot(stream_request(
            "Connect",
            json!({"process":{"tag":"stream-input-test"}}),
        ))
        .await
        .unwrap();
    let frames = all_frames(response.into_body().collect().await.unwrap().to_bytes());
    assert!(frames
        .iter()
        .any(|frame| frame["event"]["end"]["exited"] == true));
}

// 验证 Process.Connect 拒绝带压缩标志的请求帧。
#[tokio::test]
async fn connect_rejects_compressed_request_frames() {
    let response = router()
        .oneshot(
            Request::post("/process.Process/Connect")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(
                    encode_frame(0x01, br#"{"process":{"pid":1}}"#).unwrap(),
                ))
                .unwrap(),
        )
        .await
        .unwrap();

    assert_eq!(response.status(), StatusCode::NOT_IMPLEMENTED);
}

// 验证并发 Start 请求中只有一个进程能占用相同标签。
#[tokio::test]
async fn concurrent_starts_cannot_claim_the_same_tag() {
    let app = router();
    let barrier = Arc::new(tokio::sync::Barrier::new(9));
    let mut starts = tokio::task::JoinSet::new();
    for _ in 0..8 {
        let app = app.clone();
        let barrier = Arc::clone(&barrier);
        starts.spawn(async move {
            barrier.wait().await;
            app.oneshot(stream_request(
                "Start",
                json!({
                    "process": {"cmd":"/bin/sleep", "args":["1"], "envs": {}},
                    "tag": "exclusive-tag",
                    "stdin": false,
                    "pty": {"size": {"cols": 80, "rows": 24}}
                }),
            ))
            .await
            .unwrap()
            .status()
        });
    }
    barrier.wait().await;

    let mut successes = 0;
    while let Some(result) = starts.join_next().await {
        match result.unwrap() {
            StatusCode::OK => successes += 1,
            StatusCode::BAD_REQUEST => {}
            status => panic!("unexpected Start status {status}"),
        }
    }
    assert_eq!(successes, 1, "only one process may claim a tag");
}

// 构造带认证和 Connect 协议头的单帧流式进程 RPC 请求。
fn stream_request(method: &str, payload: Value) -> Request<Body> {
    Request::post(format!("/process.Process/{method}"))
        .header(CONTENT_TYPE, "application/connect+json")
        .header("Connect-Protocol-Version", "1")
        .header("Authorization", common::basic_auth_header())
        .body(Body::from(
            encode_frame(0, payload.to_string().as_bytes()).unwrap(),
        ))
        .unwrap()
}

// 发送一元进程 RPC 并解码状态码和 JSON 响应体。
async fn unary(app: axum::Router, method: &str, payload: Value) -> (StatusCode, Value) {
    let response = app
        .oneshot(
            Request::post(format!("/process.Process/{method}"))
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
    (
        status,
        if body.is_empty() {
            json!({})
        } else {
            serde_json::from_slice(&body).unwrap()
        },
    )
}

// 从 Start Connect 帧中提取新进程 PID。
fn start_pid(bytes: bytes::Bytes) -> Option<u64> {
    let frame = decode_frame(&bytes).ok()?;
    serde_json::from_slice::<Value>(&frame.payload).ok()?["event"]["start"]["pid"].as_u64()
}

// 将连续 Connect 帧拆分为普通事件或流结束 JSON 值。
fn all_frames(bytes: bytes::Bytes) -> Vec<Value> {
    let mut remaining = bytes.as_ref();
    let mut frames = Vec::new();
    while !remaining.is_empty() {
        let length = u32::from_be_bytes(remaining[1..5].try_into().unwrap()) as usize;
        let frame = decode_frame(&remaining[..5 + length]).unwrap();
        frames.push(if frame.flags == END_STREAM_FLAG {
            json!({"end": serde_json::from_slice::<Value>(&frame.payload).unwrap()})
        } else {
            serde_json::from_slice(&frame.payload).unwrap()
        });
        remaining = &remaining[5 + length..];
    }
    frames
}

// 验证 Connect-Timeout-Ms 会终止受管进程并产生结束事件。
#[tokio::test]
async fn process_timeout_terminates_and_reaps_the_process() {
    let response = router()
        .oneshot(
            Request::post("/process.Process/Start")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Connect-Timeout-Ms", "10")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(
                    encode_frame(
                        0,
                        json!({
                            "process": {"cmd": "/bin/sleep", "args": ["30"], "envs": {}},
                            "stdin": false
                        })
                        .to_string()
                        .as_bytes(),
                    )
                    .unwrap(),
                ))
                .unwrap(),
        )
        .await
        .unwrap();

    let frames = all_frames(response.into_body().collect().await.unwrap().to_bytes());
    let end = frames
        .iter()
        .find_map(|frame| frame["event"].get("end"))
        .expect("timed-out process must have an EndEvent");
    assert_eq!(end["exitCode"], 143);
    assert!(
        end.get("exited").is_none(),
        "proto JSON omits the false exited field: {end}"
    );
    assert_eq!(end["error"], "terminated by signal 15");
}

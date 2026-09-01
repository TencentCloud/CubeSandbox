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

// 验证 Process.Start 流依次传递 stdout、stderr、结束事件和结束帧。
#[tokio::test]
async fn process_start_streams_stdout_stderr_exit_and_connect_end_stream() {
    let payload = json!({
        "process": {
            "cmd": "/bin/sh",
            "args": ["-c", "printf stdout; printf stderr >&2"],
            "envs": {}
        },
        "stdin": false
    });
    let response = router()
        .oneshot(
            Request::post("/process.Process/Start")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(
                    encode_frame(0, payload.to_string().as_bytes()).unwrap(),
                ))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    let frames = split_frames(&bytes);
    assert!(frames
        .iter()
        .any(|payload| payload
            == &json!({"event":{"start":{"pid": payload["event"]["start"]["pid"]}}})));
    assert!(frames
        .iter()
        .any(|payload| payload == &json!({"event":{"data":{"stdout":"c3Rkb3V0"}}})));
    assert!(frames
        .iter()
        .any(|payload| payload == &json!({"event":{"data":{"stderr":"c3RkZXJy"}}})));
    assert!(frames
        .iter()
        .any(|payload| payload["event"]["end"]["exited"] == true));
    assert_eq!(frames.last().unwrap(), &json!({"end": {}}));
}

// 验证进程结束事件不会抢在所有 stdout 和 stderr 管道输出之前发出。
#[tokio::test]
async fn process_start_drains_pipe_output_before_streaming_the_end_event() {
    let app = router();
    for attempt in 0..100 {
        let payload = json!({
            "process": {
                "cmd": "/bin/sh",
                "args": ["-c", "printf stdout; printf stderr >&2"],
                "envs": {}
            },
            "stdin": false
        });
        let response = app
            .clone()
            .oneshot(
                Request::post("/process.Process/Start")
                    .header(CONTENT_TYPE, "application/connect+json")
                    .header("Connect-Protocol-Version", "1")
                    .header("Authorization", common::basic_auth_header())
                    .body(Body::from(
                        encode_frame(0, payload.to_string().as_bytes()).unwrap(),
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK, "attempt {attempt}");

        let frames = split_frames(&response.into_body().collect().await.unwrap().to_bytes());
        assert!(
            frames
                .iter()
                .any(|frame| frame["event"]["data"]["stdout"] == "c3Rkb3V0"),
            "attempt {attempt} lost stdout: {frames:?}"
        );
        assert!(
            frames
                .iter()
                .any(|frame| frame["event"]["data"]["stderr"] == "c3RkZXJy"),
            "attempt {attempt} lost stderr: {frames:?}"
        );
    }
}

// 验证空闲进程的 Start 流会按客户端请求发送保活事件。
#[tokio::test]
async fn process_start_sends_keepalives_during_idle_periods() {
    let payload = json!({
        "process": {"cmd":"/bin/sleep", "args":["2"], "envs": {}},
        "stdin": false
    });
    let response = router()
        .oneshot(
            Request::post("/process.Process/Start")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Keepalive-Ping-Interval", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(
                    encode_frame(0, payload.to_string().as_bytes()).unwrap(),
                ))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let mut stream = response.into_body().into_data_stream();
    let start = decode_frame(&stream.next().await.unwrap().unwrap()).unwrap();
    assert!(
        serde_json::from_slice::<serde_json::Value>(&start.payload).unwrap()["event"]["start"]
            ["pid"]
            .is_number()
    );

    let keepalive = tokio::time::timeout(std::time::Duration::from_secs(2), stream.next())
        .await
        .expect("idle process keepalive before timeout")
        .unwrap()
        .unwrap();
    let keepalive = decode_frame(&keepalive).unwrap();
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&keepalive.payload).unwrap(),
        json!({"event":{"keepalive": {}}})
    );
}

// 验证普通管道进程会继承请求中指定的环境变量。
#[tokio::test]
async fn process_start_preserves_request_environment_for_pipe_processes() {
    let payload = json!({
        "process": {
            "cmd": "/bin/sh",
            "args": ["-c", "printf \"$CUBE_TEST_MARKER\""],
            "envs": {"CUBE_TEST_MARKER": "request-env"}
        },
        "stdin": false
    });
    let response = router()
        .oneshot(
            Request::post("/process.Process/Start")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(
                    encode_frame(0, payload.to_string().as_bytes()).unwrap(),
                ))
                .unwrap(),
        )
        .await
        .unwrap();

    let frames = split_frames(&response.into_body().collect().await.unwrap().to_bytes());
    assert!(frames
        .iter()
        .any(|frame| { frame["event"]["data"]["stdout"] == "cmVxdWVzdC1lbnY=" }));
}

// 将连续 Connect 帧拆分为普通事件或流结束 JSON 值。
fn split_frames(bytes: &[u8]) -> Vec<Value> {
    let mut frames = Vec::new();
    let mut remaining = bytes;
    while !remaining.is_empty() {
        let size = u32::from_be_bytes(remaining[1..5].try_into().unwrap()) as usize;
        let frame = decode_frame(&remaining[..5 + size]).unwrap();
        if frame.flags == END_STREAM_FLAG {
            frames.push(json!({"end": serde_json::from_slice::<Value>(&frame.payload).unwrap()}));
        } else {
            frames.push(serde_json::from_slice(&frame.payload).unwrap());
        }
        remaining = &remaining[5 + size..];
    }
    frames
}

// 验证 Process.Start 将环境变量和工作目录传给子进程，并保留非零退出码。
#[tokio::test]
async fn process_start_applies_env_and_cwd_and_reports_nonzero_exit() {
    let directory = tempfile::tempdir().unwrap();
    let payload = json!({
        "process": {
            "cmd": "/bin/sh",
            "args": ["-c", "test \"$CUBE_TEST_MARKER\" = marker && test \"$PWD\" = \"$EXPECTED_CWD\"; exit 7"],
            "envs": {
                "CUBE_TEST_MARKER": "marker",
                "EXPECTED_CWD": directory.path()
            },
            "cwd": directory.path()
        },
        "stdin": false
    });
    let response = router()
        .oneshot(
            Request::post("/process.Process/Start")
                .header(CONTENT_TYPE, "application/connect+json")
                .header("Connect-Protocol-Version", "1")
                .header("Authorization", common::basic_auth_header())
                .body(Body::from(
                    encode_frame(0, payload.to_string().as_bytes()).unwrap(),
                ))
                .unwrap(),
        )
        .await
        .unwrap();

    let frames = split_frames(&response.into_body().collect().await.unwrap().to_bytes());
    assert!(frames
        .iter()
        .any(|frame| frame["event"]["end"]["exitCode"] == 7));
}

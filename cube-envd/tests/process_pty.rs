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

// 验证 PTY 进程可在退出前调整尺寸并接收输入。
#[tokio::test]
async fn pty_start_accepts_input_and_resizes_before_exiting() {
    let app = router();
    let response = app
        .clone()
        .oneshot(stream_request(
            "Start",
            json!({
                "process": {"cmd":"/bin/sh", "args":["-c", "read line; printf 'pty:%s' \"$line\""], "envs": {}},
                "pty": {"size":{"cols":80,"rows":24}}
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let mut stream = response.into_body().into_data_stream();
    let pid = start_pid(stream.next().await.unwrap().unwrap()).unwrap();

    let (status, _) = unary(
        app.clone(),
        "Update",
        json!({"process":{"pid":pid},"pty":{"size":{"cols":100,"rows":30}}}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    let (status, _) = unary(
        app,
        "SendInput",
        json!({"process":{"pid":pid},"input":{"pty":"aGVsbG8K"}}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    let bytes: Vec<Result<bytes::Bytes, axum::Error>> = StreamExt::collect(&mut stream).await;
    let bytes = bytes.into_iter().collect::<Result<Vec<_>, _>>().unwrap();
    let bytes = bytes.concat();
    let frames = frames(&bytes);
    assert!(frames
        .iter()
        .any(|frame| frame["event"]["data"]["pty"].as_str().is_some()));
    assert!(frames
        .iter()
        .any(|frame| frame["event"]["end"]["exited"] == true));
    assert_eq!(frames.last(), Some(&json!({"end": {}})));
}

// 验证普通进程超时并回收后，新的 PTY 进程仍能及时启动。
#[tokio::test]
async fn pty_start_returns_promptly_after_a_timed_out_pipe_process() {
    let app = router();
    let mut timeout_request = stream_request(
        "Start",
        json!({
            "process": {"cmd": "/bin/sleep", "args": ["30"], "envs": {}},
            "stdin": false
        }),
    );
    timeout_request
        .headers_mut()
        .insert("Connect-Timeout-Ms", "10".parse().unwrap());
    let timeout_response = app.clone().oneshot(timeout_request).await.unwrap();
    assert_eq!(timeout_response.status(), StatusCode::OK);
    let timeout_frames = frames(
        &timeout_response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes(),
    );
    assert!(timeout_frames
        .iter()
        .any(|frame| frame["event"].get("end").is_some()));

    let pty_response = tokio::time::timeout(
        std::time::Duration::from_secs(1),
        app.oneshot(stream_request(
            "Start",
            json!({
                "process": {"cmd": "/bin/sh", "args": ["-c", "read line"], "envs": {}},
                "pty": {"size": {"cols": 80, "rows": 24}}
            }),
        )),
    )
    .await
    .expect("PTY Start must not block after a timed-out pipe process")
    .unwrap();
    assert_eq!(pty_response.status(), StatusCode::OK);
}

// 验证 PTY 进程被信号终止时按 128 + N 上报终态，与管道路径形状一致。
#[tokio::test]
async fn pty_signal_termination_reports_the_same_end_shape_as_pipe_processes() {
    let app = router();
    let response = app
        .clone()
        .oneshot(stream_request(
            "Start",
            json!({
                "process": {"cmd": "/bin/sleep", "args": ["30"], "envs": {}},
                "pty": {"size": {"cols": 80, "rows": 24}}
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let mut stream = response.into_body().into_data_stream();
    let pid = start_pid(stream.next().await.unwrap().unwrap()).unwrap();

    let (status, _) = unary(
        app,
        "SendSignal",
        json!({"process":{"pid":pid},"signal":"SIGNAL_SIGKILL"}),
    )
    .await;
    assert_eq!(status, StatusCode::OK);

    let bytes: Vec<Result<bytes::Bytes, axum::Error>> = StreamExt::collect(&mut stream).await;
    let bytes = bytes.into_iter().collect::<Result<Vec<_>, _>>().unwrap();
    let bytes = bytes.concat();
    let frames = frames(&bytes);
    let end = frames
        .iter()
        .find_map(|frame| frame["event"]["end"].as_object())
        .expect("PTY Start stream must end with an end event");

    // SIGKILL = 9，与 process_control 的管道路径断言保持同一约定。
    assert_eq!(end["exitCode"], 137, "end event: {end:?}");
    assert_eq!(
        end["status"], "terminated by signal 9",
        "end event: {end:?}"
    );
    assert_eq!(end["error"], "terminated by signal 9", "end event: {end:?}");
    assert!(
        end.get("exited").is_none(),
        "proto JSON omits the false exited field: {end:?}"
    );
}

// 验证 PTY 进程正常退出时与管道路径一致地报告 exit status。
#[tokio::test]
async fn pty_normal_exit_reports_exit_status_and_exited() {
    let app = router();
    let response = app
        .oneshot(stream_request(
            "Start",
            json!({
                "process": {"cmd": "/bin/sh", "args": ["-c", "exit 3"], "envs": {}},
                "pty": {"size": {"cols": 80, "rows": 24}}
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    let frames = frames(&bytes);
    let end = frames
        .iter()
        .find_map(|frame| frame["event"]["end"].as_object())
        .expect("PTY Start stream must end with an end event");

    assert_eq!(end["exitCode"], 3, "end event: {end:?}");
    assert_eq!(end["exited"], true, "end event: {end:?}");
    assert_eq!(end["status"], "exit status 3", "end event: {end:?}");
    assert!(
        end.get("error").is_none(),
        "normal exit must not carry an error: {end:?}"
    );
}

// 验证孙进程持有 PTY slave 时，End 仍会在宽限内发出并完成流。
//
// PTY 读线程在孙进程持有 slave 期间会阻塞在 read 上且无法被 abort 中断；收尾因此
// 依赖"宽限超时后置中断标志并封住输出"，而不是等待该线程退出。
#[cfg(unix)]
#[tokio::test]
async fn pty_end_is_published_when_a_grandchild_holds_the_slave() {
    let app = router();
    let response = app
        .oneshot(stream_request(
            "Start",
            json!({
                "process": {
                    "cmd": "/bin/sh",
                    "args": ["-c", "sleep 30 & printf done"],
                    "envs": {}
                },
                "pty": {"size": {"cols": 80, "rows": 24}}
            }),
        ))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);

    let bytes = tokio::time::timeout(
        std::time::Duration::from_secs(10),
        response.into_body().collect(),
    )
    .await
    .expect("PTY End must not wait for the grandchild holding the slave")
    .unwrap()
    .to_bytes();
    let frames = frames(&bytes);
    assert!(
        frames.iter().any(|frame| frame["event"]["end"].is_object()),
        "PTY stream must end with an end event: {frames:?}"
    );
    assert_eq!(frames.last(), Some(&json!({"end": {}})));
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
fn frames(bytes: &[u8]) -> Vec<Value> {
    let mut frames = Vec::new();
    let mut remaining = bytes;
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

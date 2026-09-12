// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

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

// 验证子进程获得上游 envd 注入的 PATH、HOME、USER 与 LOGNAME。
#[tokio::test]
async fn process_start_injects_path_home_user_and_logname() {
    let current = nix::unistd::User::from_uid(nix::unistd::getuid())
        .expect("look up current local user")
        .expect("current uid has a passwd entry");
    let payload = json!({
        "process": {
            "cmd": "/bin/sh",
            "args": [
                "-c",
                "test \"$HOME\" = \"$EXPECTED_HOME\" \
                 && test \"$USER\" = \"$EXPECTED_USER\" \
                 && test \"$LOGNAME\" = \"$EXPECTED_USER\" \
                 && test -n \"$PATH\"; exit 7"
            ],
            "envs": {
                "EXPECTED_HOME": current.dir.display().to_string(),
                "EXPECTED_USER": current.name
            }
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

    let frames = split_frames(&response.into_body().collect().await.unwrap().to_bytes());
    assert!(
        frames
            .iter()
            .any(|frame| frame["event"]["end"]["exitCode"] == 7),
        "child did not observe the injected PATH/HOME/USER/LOGNAME: {frames:?}"
    );
}

// 验证请求级 envs 可以覆盖 envd 注入的基础环境变量。
#[tokio::test]
async fn process_start_request_envs_override_injected_base_environment() {
    let payload = json!({
        "process": {
            "cmd": "/bin/sh",
            "args": ["-c", "test \"$HOME\" = \"/tmp/cube-envd-home-override\"; exit 7"],
            "envs": {"HOME": "/tmp/cube-envd-home-override"}
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
    assert!(
        frames
            .iter()
            .any(|frame| frame["event"]["end"]["exitCode"] == 7),
        "request envs did not override the injected base environment: {frames:?}"
    );
}

// 验证未指定 cwd 时子进程在目标用户的主目录下启动。
#[tokio::test]
async fn process_start_defaults_to_the_user_home_directory() {
    let current = nix::unistd::User::from_uid(nix::unistd::getuid())
        .expect("look up current local user")
        .expect("current uid has a passwd entry");
    let payload = json!({
        "process": {
            "cmd": "/bin/sh",
            "args": ["-c", "test \"$PWD\" = \"$EXPECTED_HOME\"; exit 7"],
            "envs": {"EXPECTED_HOME": current.dir.display().to_string()}
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

    let frames = split_frames(&response.into_body().collect().await.unwrap().to_bytes());
    assert!(
        frames
            .iter()
            .any(|frame| frame["event"]["end"]["exitCode"] == 7),
        "process did not start in the user home directory: {frames:?}"
    );
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

// 验证 Start 的结束事件在孙进程仍持有输出管道时也会及时送达。
//
// `sh -c 'sleep 30 & echo done'` 中 sleep 继承 stdout 管道写端，sh 退出后
// 管道不 EOF——若无宽限机制 End 会被无限拖住（直到 sleep 退出或 SDK 超时）。
// 断言 End 在远小于 sleep 时长内到达，且 stdout 的 "done" 未被截断。
#[tokio::test]
async fn process_start_publishes_end_promptly_when_a_grandchild_holds_the_pipe() {
    let payload = json!({
        "process": {
            "cmd": "/bin/sh",
            "args": ["-c", "sleep 30 & printf done"],
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

    let bytes = tokio::time::timeout(
        std::time::Duration::from_secs(5),
        response.into_body().collect(),
    )
    .await
    .expect("stream must end long before the grandchild sleep exits")
    .unwrap()
    .to_bytes();
    let frames = split_frames(&bytes);

    // "done" 输出保留（grace 内已 flush），End 存在，且流以结束帧收尾。
    assert!(frames
        .iter()
        .any(|frame| frame["event"]["data"]["stdout"] == "ZG9uZQ=="));
    assert!(frames
        .iter()
        .any(|frame| frame["event"]["end"]["exited"] == true));
    assert_eq!(frames.last().unwrap(), &json!({"end": {}}));
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

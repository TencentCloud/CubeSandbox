// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// 回归测试：Connect 与进程收尾（live → terminal）交错时，任何成功建立的
// 连接流都必须以 End 事件收尾——live 挂载经 End 槽合成，或回放终端记录——
// 不允许出现"无 End 的空流"或本应命中的回放丢失（PID 复用记录遮蔽防护）。
use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, Request, StatusCode},
    Router,
};
use cube_envd::{
    app::router,
    connect::{decode_frame, encode_frame, END_STREAM_FLAG},
};
use http_body_util::BodyExt;
use serde_json::{json, Value};
use tower::ServiceExt;

mod common;

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

/// 启动 `/bin/true` 并返回其完整 Start 流帧（首个事件帧携带 PID）。
async fn start_true(app: Router, tag: &str) -> Vec<Value> {
    let payload = json!({
        "process": {"cmd": "/bin/true", "args": [], "envs": {}},
        "stdin": false,
        "tag": tag
    });
    let response = app
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
    split_frames(&response.into_body().collect().await.unwrap().to_bytes())
}

/// 按选择器发起 Connect，返回帧序列或 HTTP 状态。
async fn connect_by(app: Router, selector: Value) -> Result<Vec<Value>, StatusCode> {
    let payload = json!({"process": selector});
    let response = app
        .oneshot(
            Request::post("/process.Process/Connect")
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
    if response.status() != StatusCode::OK {
        return Err(response.status());
    }
    Ok(split_frames(
        &response.into_body().collect().await.unwrap().to_bytes(),
    ))
}

fn assert_ended_with_exit(frames: &[Value], label: &str) {
    let end_events: Vec<&Value> = frames
        .iter()
        .filter(|frame| frame["event"]["end"]["exited"] == true)
        .collect();
    assert!(
        !end_events.is_empty(),
        "{label}: connect stream must deliver an End event, got {frames:?}"
    );
    assert_eq!(
        frames.last().unwrap(),
        &json!({"end": {}}),
        "{label}: stream must terminate with the Connect end frame"
    );
}

// 已结束进程在保留窗口内按 PID 与标签回放：必须稳定返回 End，绝不 NotFound。
#[tokio::test]
async fn ended_process_replays_by_pid_and_tag_with_an_end_event() {
    let app = router();
    for attempt in 0..60 {
        let tag = format!("replay-{attempt}");
        let start_frames = start_true(app.clone(), &tag).await;
        let pid = start_frames[0]["event"]["start"]["pid"]
            .as_u64()
            .expect("start frame carries pid") as u32;

        // 按 PID 回放。
        let frames = connect_by(app.clone(), json!({"pid": pid}))
            .await
            .expect("pid replay must not be NotFound within the retention window");
        assert_ended_with_exit(&frames, &format!("attempt {attempt} pid {pid}"));

        // 按标签回放（标签在收尾时解绑，须经终端记录解析）。
        let frames = connect_by(app.clone(), json!({"tag": tag}))
            .await
            .expect("tag replay must not be NotFound within the retention window");
        assert_ended_with_exit(&frames, &format!("attempt {attempt} tag {tag}"));
    }
}

// 竞态压测：进程快速退出期间并发发起 Connect（可能撞上 live → terminal
// 转换窗口）。任何成功建立的连接都必须以 End 收尾；仅有在进程注册完成前
// 抢先到达的 Connect 允许返回 NotFound（它没有挂到任何状态上）。
#[tokio::test]
async fn connect_racing_fast_exit_always_delivers_an_end_event() {
    for attempt in 0..150 {
        let tag = format!("race-{attempt}");
        let app = router();

        // Start 与 Connect 并发发出，尽量让 Connect 落在进程存活/收尾窗口内。
        let start_app = app.clone();
        let tag_for_start = tag.clone();
        let start_task = tokio::spawn(async move { start_true(start_app, &tag_for_start).await });

        match connect_by(app.clone(), json!({"tag": tag})).await {
            Ok(frames) => {
                assert_ended_with_exit(&frames, &format!("attempt {attempt}"));
            }
            Err(StatusCode::NOT_FOUND) => {
                // Connect 在进程完成注册前到达：未挂载任何状态，属正常抢先。
            }
            Err(status) => panic!("attempt {attempt}: unexpected status {status}"),
        }

        let start_frames = start_task.await.expect("start task completes");
        assert_ended_with_exit(&start_frames, &format!("attempt {attempt} start stream"));
    }
}

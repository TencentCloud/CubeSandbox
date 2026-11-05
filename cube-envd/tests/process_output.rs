// SPDX-License-Identifier: Apache-2.0

use base64::Engine;
use cube_envd::server::ServerPhase;
use reqwest::Client;
use serde_json::{json, Value};
use std::time::Duration;
#[path = "support/process.rs"]
mod process;
mod support;
use process::Stream;

#[tokio::test]
async fn immediate_output_and_exit_always_start_first_and_end_once() {
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    for _ in 0..20 {
        let mut stream = Stream::open(
            &client,
            port,
            "Start",
            json!({"process":{
                "cmd":"/bin/sh", "args":["-c","printf out; printf err >&2; exit 7"]
            }}),
        )
        .await;
        let first = stream.next().await;
        assert!(first.1["event"]["start"]["pid"].as_u64().unwrap() > 0);
        let (mut stdout, mut stderr) = (Vec::new(), Vec::new());
        loop {
            let (flag, value) = stream.next().await;
            assert_eq!(flag, 0, "expected Data/End before trailer: {value}");
            let event = &value["event"];
            if let Some(end) = event.get("end") {
                assert_eq!(end["exitCode"], 7);
                assert_eq!(end["exited"], true);
                assert_eq!(end["status"], "exit status 7");
                assert_eq!(end["error"], "exit status 7");
                break;
            }
            for (name, bytes) in [("stdout", &mut stdout), ("stderr", &mut stderr)] {
                if let Some(data) = event["data"][name].as_str() {
                    bytes.extend(
                        base64::engine::general_purpose::STANDARD
                            .decode(data)
                            .unwrap(),
                    );
                }
            }
        }
        assert_eq!(stdout, b"out");
        assert_eq!(stderr, b"err");
        assert_eq!(stream.next().await, (2, json!({})));
    }
    server.abort();
}

async fn listed(client: &Client, port: u16) -> Vec<Value> {
    client
        .post(format!("http://127.0.0.1:{port}/process.Process/List"))
        .header("connect-protocol-version", "1")
        .json(&json!({}))
        .send()
        .await
        .unwrap()
        .json::<Value>()
        .await
        .unwrap()["processes"]
        .as_array()
        .cloned()
        .unwrap_or_default()
}
async fn wait_removed(client: &Client, port: u16) {
    tokio::time::timeout(Duration::from_secs(3), async {
        while !listed(client, port).await.is_empty() {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
}

#[tokio::test]
async fn connect_binds_live_identity_without_replay_and_terminal_releases_tag_before_drain() {
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    for protobuf in [false, true] {
        let directory = tempfile::tempdir().unwrap();
        let release = directory.path().join("release");
        let mut original = Stream::encoded(&client, port, "Start", json!({"tag":"", "process":{
        "cmd":"/bin/sh", "args":["-c","printf history; while [ ! -e \"$1\" ]; do /bin/sleep .01; done; printf future; /bin/sleep .5 & exit 4", "test", release]
    }}), protobuf).await;
        let start = original.next().await;
        let pid = start.1["event"]["start"]["pid"].as_u64().unwrap();
        assert_eq!(
            original.next().await.1["event"]["data"]["stdout"],
            "aGlzdG9yeQ=="
        );
        drop(original);
        let mut by_tag = Stream::encoded(
            &client,
            port,
            "Connect",
            json!({"process":{"tag":""}}),
            protobuf,
        )
        .await;
        assert_eq!(by_tag.next().await, start);
        let mut by_pid = Stream::encoded(
            &client,
            port,
            "Connect",
            json!({"process":{"pid":pid}}),
            protobuf,
        )
        .await;
        assert_eq!(by_pid.next().await, start);
        std::fs::write(release, "").unwrap();
        wait_removed(&client, port).await;
        for selector in [json!({"pid":pid}), json!({"tag":""})] {
            let mut missing = Stream::encoded(
                &client,
                port,
                "Connect",
                json!({"process":selector}),
                protobuf,
            )
            .await;
            assert_eq!(missing.next().await.1["error"]["code"], "not_found");
        }
        let mut reused = Stream::encoded(
            &client,
            port,
            "Start",
            json!({"tag":"", "process":{"cmd":"/bin/true"}}),
            protobuf,
        )
        .await;
        assert_ne!(reused.next().await.1["event"]["start"]["pid"], pid);
        for mut bound in [by_tag, by_pid] {
            assert_eq!(bound.next().await.1["event"]["data"]["stdout"], "ZnV0dXJl");
            let terminal = tokio::time::timeout(Duration::from_secs(1), bound.next())
                .await
                .expect("End follows descendant pipe closure");
            assert_eq!(terminal.1["event"]["end"]["exitCode"], 4);
            assert_eq!(bound.next().await, (2, json!({})));
        }
    }
    server.abort();
}

#[tokio::test]
async fn keepalive_defaults_bounds_and_data_reset() {
    use futures::{FutureExt, StreamExt};
    use tower::ServiceExt;
    for method in ["Start", "Connect"] {
        for (header, seconds) in [
            (None, 90),
            (Some("bad"), 90),
            (Some("50"), 50),
            (Some("1"), 1),
            (Some("0"), 0),
            (Some("-1"), 0),
            (Some("9223372037"), 0),
            (Some("9999999999999999999999999999"), 0),
        ] {
            let (port, state, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
            let router = cube_envd::transport::build_router(state);
            let directory = tempfile::tempdir().unwrap();
            let release = directory.path().join("data");
            let process = json!({"process":{"cmd":"/bin/sh","args":["-c",
            "while [ ! -e \"$1\" ]; do /bin/sleep .01; done; printf reset; /bin/sleep 1", "test", release]}});
            let request_value = if method == "Connect" {
                let client = Client::builder().no_proxy().build().unwrap();
                let mut original = Stream::open(&client, port, "Start", process.clone()).await;
                let pid = original.next().await.1["event"]["start"]["pid"].clone();
                json!({"process":{"pid":pid}})
            } else {
                process
            };
            let payload = serde_json::to_vec(&request_value).unwrap();
            let mut body = vec![0];
            body.extend_from_slice(&(payload.len() as u32).to_be_bytes());
            body.extend(payload);
            let mut request = http::Request::post(format!("/process.Process/{method}"))
                .header("content-type", "application/connect+json")
                .header("connect-protocol-version", "1");
            if let Some(header) = header {
                request = request.header("keepalive-ping-interval", header);
            }
            let response = router
                .oneshot(request.body(axum::body::Body::from(body)).unwrap())
                .await
                .unwrap();
            let mut stream = response.into_body().into_data_stream();
            let first = stream.next().await.unwrap().unwrap();
            if seconds == 0 {
                assert_eq!(first[0], 2, "header={header:?}");
                assert_eq!(
                    serde_json::from_slice::<Value>(&first[5..]).unwrap()["error"]["code"],
                    "invalid_argument"
                );
                std::fs::write(&release, "").unwrap();
                server.abort();
                continue;
            }
            assert_eq!(first[0], 0);
            tokio::time::pause();
            assert!(stream.next().now_or_never().is_none());
            tokio::time::advance(Duration::from_secs(seconds - 1)).await;
            assert!(
                stream.next().now_or_never().is_none(),
                "early keepalive {header:?}"
            );
            tokio::time::advance(Duration::from_millis(1002)).await;
            let frame = stream
                .next()
                .now_or_never()
                .expect("keepalive due")
                .unwrap()
                .unwrap();
            assert!(
                serde_json::from_slice::<Value>(&frame[5..]).unwrap()["event"]
                    .get("keepalive")
                    .is_some()
            );
            tokio::time::advance(Duration::from_millis(500)).await;
            std::fs::write(&release, "").unwrap();
            // OS child execution uses real time, independently of the guest timer.
            let mut received = false;
            for _ in 0..100 {
                if let Some(Some(Ok(frame))) = stream.next().now_or_never() {
                    assert_eq!(
                        serde_json::from_slice::<Value>(&frame[5..]).unwrap()["event"]["data"]
                            ["stdout"],
                        "cmVzZXQ="
                    );
                    received = true;
                    break;
                }
                std::thread::sleep(Duration::from_millis(2));
                tokio::task::yield_now().await;
            }
            assert!(received);
            tokio::time::advance(Duration::from_millis(seconds * 1000 - 1)).await;
            assert!(
                stream.next().now_or_never().is_none(),
                "Data must reset timer"
            );
            tokio::time::advance(Duration::from_millis(3)).await;
            assert!(
                stream.next().now_or_never().is_some(),
                "reset keepalive due"
            );
            tokio::time::resume();
            drop(stream);
            server.abort();
        }
    }
}

#[tokio::test]
async fn stalled_subscriber_applies_backpressure_and_preserves_complete_output() {
    use futures::StreamExt;
    use tower::ServiceExt;
    let (port, state, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    let directory = tempfile::tempdir().unwrap();
    let release = directory.path().join("release");
    let payload = serde_json::to_vec(&json!({"tag":"pressure", "process":{"cmd":"python3","args":["-c",
        "import os,sys,time\nwhile not os.path.exists(sys.argv[1]): time.sleep(.005)\nfor i in range(128):\n os.write(1,bytes([i])*8192); time.sleep(.002)\ntime.sleep(.1)", release]}})).unwrap();
    let mut body = vec![0];
    body.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    body.extend(payload);
    let response = cube_envd::transport::build_router(state.clone())
        .oneshot(
            http::Request::post("/process.Process/Start")
                .header("content-type", "application/connect+json")
                .header("connect-protocol-version", "1")
                .body(axum::body::Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap();
    // A body which is deliberately not polled models a completely stalled
    // transport without relying on platform TCP send-buffer sizes.
    let mut stalled = response.into_body().into_data_stream();
    let first = stalled.next().await.unwrap().unwrap();
    assert_eq!(first[0], 0);
    let mut healthy = Stream::open(
        &client,
        port,
        "Connect",
        json!({"process":{"tag":"pressure"}}),
    )
    .await;
    assert!(healthy.next().await.1["event"].get("start").is_some());
    std::fs::write(release, "").unwrap();
    let mut healthy_task = tokio::spawn(async move {
        let mut received = Vec::new();
        loop {
            let (flag, value) = healthy.next().await;
            assert_eq!(flag, 0, "healthy subscriber must progress: {value}");
            if let Some(end) = value["event"].get("end") {
                assert_eq!(end["exited"], true);
                assert_eq!(end["exitCode"].as_i64().unwrap_or(0), 0);
                break;
            }
            received.extend(
                base64::engine::general_purpose::STANDARD
                    .decode(value["event"]["data"]["stdout"].as_str().unwrap())
                    .unwrap(),
            );
        }
        assert_eq!(healthy.next().await, (2, json!({})));
        received
    });
    assert!(
        tokio::time::timeout(Duration::from_millis(500), &mut healthy_task)
            .await
            .is_err(),
        "slow subscriber must apply backpressure"
    );
    assert!(state.lifecycle.is_ready());
    let expected: Vec<u8> = (0..128_u8)
        .flat_map(|value| std::iter::repeat_n(value, 8192))
        .collect();
    let mut prefix = Vec::new();
    let mut wire = Vec::new();
    while let Some(chunk) = stalled.next().await {
        wire.extend(chunk.unwrap());
    }
    let mut body = wire.as_slice();
    let mut ended = false;
    while !body.is_empty() {
        let length = u32::from_be_bytes(body[1..5].try_into().unwrap()) as usize;
        let value: Value = serde_json::from_slice(&body[5..5 + length]).unwrap();
        if body[0] == 2 {
            assert_eq!(value, json!({}));
            assert_eq!(body.len(), 5 + length);
        } else if let Some(end) = value["event"].get("end") {
            assert_eq!(end["exitCode"].as_i64().unwrap_or(0), 0);
            ended = true;
        } else {
            prefix.extend(
                base64::engine::general_purpose::STANDARD
                    .decode(value["event"]["data"]["stdout"].as_str().unwrap())
                    .unwrap(),
            );
        }
        body = &body[5 + length..];
    }
    assert!(ended);
    assert_eq!(prefix, expected);
    assert_eq!(healthy_task.await.unwrap(), expected);
    wait_removed(&client, port).await;
    assert!(state.lifecycle.is_ready());
    server.abort();
}

#[tokio::test]
async fn pipe_eof_does_not_end_a_live_leader_and_signal_exit_is_authoritative() {
    let (port, _, server) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = Client::builder().no_proxy().build().unwrap();
    let directory = tempfile::tempdir().unwrap();
    let release = directory.path().join("release");
    let mut stream = Stream::open(&client, port, "Start", json!({"process":{"cmd":"/bin/sh", "args":["-c",
        "exec 1>&- 2>&-; while [ ! -e \"$1\" ]; do /bin/sleep .01; done; kill -TERM $$", "test", release]}})).await;
    let pid = stream.next().await.1["event"]["start"]["pid"]
        .as_u64()
        .unwrap();
    tokio::time::sleep(Duration::from_millis(30)).await;
    assert_eq!(listed(&client, port).await[0]["pid"], pid);
    std::fs::write(release, "").unwrap();
    let end = stream.next().await.1["event"]["end"].clone();
    assert_eq!(end["exitCode"], -1);
    assert!(!end["exited"].as_bool().unwrap_or(false));
    assert_eq!(end["status"], "signal: terminated");
    assert_eq!(end["error"], "signal: terminated");
    assert_eq!(stream.next().await, (2, json!({})));
    wait_removed(&client, port).await;
    server.abort();
}

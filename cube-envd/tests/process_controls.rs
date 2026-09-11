// SPDX-License-Identifier: Apache-2.0

use base64::{engine::general_purpose::STANDARD, Engine};
use reqwest::Client;
use serde_json::json;
use std::time::Duration;
#[path = "support/process.rs"]
mod process;
mod support;
use process::{unary, unary_proto, Stream};

impl Stream {
    async fn output(&mut self, expected: &[u8]) {
        let mut bytes = vec![];
        while bytes.len() < expected.len() {
            let (flag, event) = self.next().await;
            assert_eq!(flag, 0, "{event}");
            bytes.extend(
                base64::engine::general_purpose::STANDARD
                    .decode(event["event"]["data"]["stdout"].as_str().unwrap())
                    .unwrap(),
            );
        }
        assert_eq!(bytes, expected);
    }
}

#[tokio::test]
async fn stdin_full_write_and_ordered_idempotent_close() {
    use cube_envd::proto::process::{
        CloseStdinRequest, CloseStdinResponse, SendInputRequest, SendInputResponse,
    };
    for protobuf in [false, true] {
        let (port, server) = support::spawn_daemon().await;
        let client = Client::builder().no_proxy().build().unwrap();
        let mut stream = Stream::encoded(
            &client,
            port,
            "Start",
            json!({"tag":"input", "stdin":true,
        "process":{"cmd":"/bin/sh","args":["-c","cat; sleep .2"]}}),
            protobuf,
        )
        .await;
        let pid = stream.next().await.1["event"]["start"]["pid"].clone();
        let input = b"complete input\n";
        let request = json!({"process":{"tag":"input"},"input":{"stdin":base64::engine::general_purpose::STANDARD.encode(input)}});
        let result = if protobuf {
            unary_proto::<SendInputRequest, SendInputResponse>(&client, port, "SendInput", request)
                .await
        } else {
            unary(&client, port, "SendInput", request).await
        };
        assert_eq!(result, (200, json!({})));
        for _ in 0..2 {
            let request = json!({"process":{"pid":pid}});
            let result = if protobuf {
                unary_proto::<CloseStdinRequest, CloseStdinResponse>(
                    &client,
                    port,
                    "CloseStdin",
                    request,
                )
                .await
            } else {
                unary(&client, port, "CloseStdin", request).await
            };
            assert_eq!(result, (200, json!({})));
        }
        let mut output = vec![];
        loop {
            let (flag, event) = stream.next().await;
            assert_eq!(flag, 0, "{event}");
            if event["event"].get("end").is_some() {
                break;
            }
            output.extend(
                base64::engine::general_purpose::STANDARD
                    .decode(event["event"]["data"]["stdout"].as_str().unwrap())
                    .unwrap(),
            );
        }
        assert_eq!(output, input);
        assert_eq!(stream.next().await, (2, json!({})));
        assert_eq!(
            unary(&client, port, "CloseStdin", json!({"process":{"pid":pid}}))
                .await
                .1["code"],
            "not_found"
        );
        assert_eq!(unary(&client, port, "List", json!({})).await.1, json!({}));
        server.abort();
    }
}

#[tokio::test]
async fn signals_target_only_live_leaders_and_term_is_not_terminal() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    let mut stream = Stream::open(&client, port, "Start", json!({"tag":"signals",
        "process":{"cmd":"python3","args":["-u","-c","import signal,time\nsignal.signal(signal.SIGTERM,lambda *_:print('caught',flush=True))\nprint('ready',flush=True)\nwhile True: time.sleep(.01)"]}})).await;
    let pid = stream.next().await.1["event"]["start"]["pid"].clone();
    stream.output(b"ready\n").await;
    for signal in [0, 1, 2, 10] {
        assert_eq!(
            unary(
                &client,
                port,
                "SendSignal",
                json!({"process":{"pid":pid},"signal":signal})
            )
            .await
            .1["code"],
            "unimplemented"
        );
    }
    assert_eq!(
        unary(
            &client,
            port,
            "SendSignal",
            json!({"process":{"pid":1},"signal":9})
        )
        .await
        .1["code"],
        "not_found"
    );
    assert_eq!(
        unary(
            &client,
            port,
            "SendSignal",
            json!({"process":{"tag":"signals"},"signal":15})
        )
        .await,
        (200, json!({}))
    );
    stream.output(b"caught\n").await;
    assert_eq!(
        unary(&client, port, "List", json!({})).await.1["processes"][0]["pid"],
        pid
    );
    assert_eq!(
        unary(
            &client,
            port,
            "SendSignal",
            json!({"process":{"pid":pid},"signal":9})
        )
        .await,
        (200, json!({}))
    );
    assert_eq!(
        stream.next().await.1["event"]["end"]["status"],
        "signal: killed"
    );
    assert_eq!(stream.next().await, (2, json!({})));
    assert_eq!(
        unary(
            &client,
            port,
            "SendSignal",
            json!({"process":{"tag":"signals"},"signal":9})
        )
        .await
        .1["code"],
        "not_found"
    );
    server.abort();
}

#[tokio::test]
async fn live_deadline_kills_immediately_and_connect_cannot_reset_it() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    let started = tokio::time::Instant::now();
    let mut stream=Stream::timed(&client,port,"Start",json!({"tag":"deadline","process":{"cmd":"python3","args":["-u","-c","import signal,time\nsignal.signal(signal.SIGTERM,lambda *_:print('unexpected TERM'))\nprint('ready')\ntime.sleep(10)"]}}),false,Some("600")).await;
    stream.next().await;
    stream.output(b"ready\n").await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    let mut connected = Stream::timed(
        &client,
        port,
        "Connect",
        json!({"process":{"tag":"deadline"}}),
        false,
        Some("10000"),
    )
    .await;
    connected.next().await;
    let end = tokio::time::timeout(Duration::from_millis(700), connected.next())
        .await
        .expect("live deadline must kill without resetting on Connect")
        .1;
    assert_eq!(end["event"]["end"]["status"], "signal: killed");
    assert!(started.elapsed() < Duration::from_millis(1000));
    assert_eq!(connected.next().await, (2, json!({})));
    assert_eq!(unary(&client, port, "List", json!({})).await.1, json!({}));
    assert_eq!(
        unary(
            &client,
            port,
            "SendInput",
            json!({"process":{"tag":"deadline"},"input":{"stdin":""}})
        )
        .await
        .1["code"],
        "not_found"
    );
    server.abort();
}

#[tokio::test]
async fn timeout_wire_values_preserve_independent_process_cleanup() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    for timeout in ["0", "-1"] {
        let mut stream = Stream::timed(
            &client,
            port,
            "Start",
            json!({"tag":"no-deadline","process":{"cmd":"/bin/sleep","args":[".2"]}}),
            false,
            Some(timeout),
        )
        .await;
        let start = stream.next().await;
        assert!(start.1["event"].get("start").is_some(), "{start:?}");
        for connect_timeout in ["0", "-1"] {
            let mut attach = Stream::timed(
                &client,
                port,
                "Connect",
                json!({"process":{"tag":"no-deadline"}}),
                false,
                Some(connect_timeout),
            )
            .await;
            assert_eq!(attach.next().await, start);
            assert_eq!(attach.next().await.1["error"]["code"], "deadline_exceeded");
        }
        assert_eq!(stream.next().await.1["error"]["code"], "deadline_exceeded");
        tokio::time::timeout(Duration::from_secs(5), async {
            while unary(&client, port, "List", json!({})).await.1 != json!({}) {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
    }
    for timeout in ["bad", "9223372036855", "9999999999999999999999999999"] {
        let mut stream = Stream::timed(
            &client,
            port,
            "Start",
            json!({"tag":"invalid-deadline","process":{"cmd":"/bin/sleep","args":["10"]}}),
            false,
            Some(timeout),
        )
        .await;
        assert_eq!(stream.next().await.1["error"]["code"], "invalid_argument");
        assert_eq!(unary(&client, port, "List", json!({})).await.1, json!({}));
    }
    server.abort();
}

#[tokio::test]
async fn live_input_validation_and_disabled_stdin_close_are_explicit() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    for enabled in [false, true] {
        let mut stream = Stream::open(
            &client,
            port,
            "Start",
            json!({"stdin":enabled,"process":{"cmd":"/bin/sleep","args":["10"]}}),
        )
        .await;
        let pid = stream.next().await.1["event"]["start"]["pid"].clone();
        for input in [json!({}), json!({"stdin":""}), json!({"pty":""})] {
            let result = unary(
                &client,
                port,
                "SendInput",
                json!({"process":{"pid":pid},"input":input}),
            )
            .await;
            if input.get("stdin").is_some() && enabled {
                assert_eq!(result, (200, json!({})));
            } else {
                assert_eq!(
                    result.1["code"],
                    if input.get("pty").is_some() || input.get("stdin").is_some() {
                        "internal"
                    } else {
                        "unimplemented"
                    }
                );
            }
        }
        for _ in 0..2 {
            assert_eq!(
                unary(&client, port, "CloseStdin", json!({"process":{"pid":pid}})).await,
                (200, json!({}))
            );
        }
        assert_eq!(
            unary(
                &client,
                port,
                "SendInput",
                json!({"process":{"pid":pid},"input":{"stdin":"eA=="}})
            )
            .await
            .1["code"],
            "internal"
        );
        assert_eq!(
            unary(
                &client,
                port,
                "SendSignal",
                json!({"process":{"pid":pid},"signal":9})
            )
            .await
            .0,
            200
        );
        assert!(stream.next().await.1["event"].get("end").is_some());
    }
    server.abort();
}

#[tokio::test]
async fn term_kill_and_deadline_do_not_signal_background_children() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    let directory = tempfile::tempdir().unwrap();
    for signal in [15, 9, 0] {
        let marker = directory.path().join(signal.to_string());
        let mut stream=Stream::timed(&client,port,"Start",json!({"tag":"leader-only","process":{"cmd":"python3","args":["-u","-c",
            format!("import os,time\nleader=os.getpid()\nif os.fork()==0:\n while os.getppid()==leader: time.sleep(.001)\n with open({:?}, 'w') as f: f.write('survived')\n os._exit(0)\nos.write(1,b'ready\\n'); time.sleep(10)", marker)]}}),false,if signal==0{Some("500")}else{None}).await;
        let pid = stream.next().await.1["event"]["start"]["pid"].clone();
        stream.output(b"ready\n").await;
        if signal == 0 {
            stream = Stream::open(&client, port, "Connect", json!({"process":{"pid":pid}})).await;
            assert!(stream.next().await.1["event"].get("start").is_some());
        }
        if signal != 0 {
            assert_eq!(
                unary(
                    &client,
                    port,
                    "SendSignal",
                    json!({"process":{"pid":pid},"signal":signal})
                )
                .await
                .0,
                200
            );
        }
        tokio::time::timeout(Duration::from_secs(5), async {
            while std::fs::read(&marker).ok().as_deref() != Some(b"survived") {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(std::fs::read(&marker).unwrap(), b"survived");
        assert_eq!(
            stream.next().await.1["event"]["end"]["status"],
            if signal == 15 {
                "signal: terminated"
            } else {
                "signal: killed"
            }
        );
        assert_eq!(stream.next().await, (2, json!({})));
        assert_eq!(unary(&client, port, "List", json!({})).await.1, json!({}));
    }
    server.abort();
}

#[tokio::test]
async fn peer_closed_stdin_without_delivery_is_internal() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    let mut stream=Stream::open(&client,port,"Start",json!({"stdin":true,"process":{"cmd":"python3","args":["-u","-c","import os,time\nos.close(0); os.write(1,b'closed\\n'); time.sleep(1)"]}})).await;
    let pid = stream.next().await.1["event"]["start"]["pid"].clone();
    stream.output(b"closed\n").await;
    for _ in 0..2 {
        assert_eq!(
            unary(
                &client,
                port,
                "SendInput",
                json!({"process":{"pid":pid},"input":{"stdin":"eA=="}})
            )
            .await
            .1["code"],
            "internal"
        );
    }
    assert_eq!(
        unary(&client, port, "CloseStdin", json!({"process":{"pid":pid}}))
            .await
            .0,
        200
    );
    assert_eq!(
        client
            .get(format!("http://127.0.0.1:{port}/health"))
            .send()
            .await
            .unwrap()
            .status()
            .as_u16(),
        204
    );
    assert!(stream.next().await.1["event"].get("end").is_some());
    server.abort();
}

#[tokio::test]
async fn stream_input_fragmentation_order_keepalive_and_eof() {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    let mut output = Stream::open(
        &client,
        port,
        "Start",
        json!({
            "process":{"cmd":"/bin/cat"},"stdin":true
        }),
    )
    .await;
    let pid = output.next().await.1["event"]["start"]["pid"].clone();
    let messages = [
        json!({"start":{"process":{"pid":pid}}}),
        json!({"keepalive":{}}),
        json!({"data":{"input":{"stdin":STANDARD.encode(b"first\n")}}}),
        json!({"data":{"input":{"stdin":STANDARD.encode(b"second\n")}}}),
    ];
    let mut wire = Vec::new();
    for message in messages {
        let payload = serde_json::to_vec(&message).unwrap();
        wire.push(0);
        wire.extend_from_slice(&(payload.len() as u32).to_be_bytes());
        wire.extend(payload);
    }
    let mut socket = tokio::net::TcpStream::connect(("127.0.0.1", port))
        .await
        .unwrap();
    socket.write_all(b"POST /process.Process/StreamInput HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/connect+json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n").await.unwrap();
    for chunk in wire.chunks(3) {
        socket
            .write_all(format!("{:x}\r\n", chunk.len()).as_bytes())
            .await
            .unwrap();
        socket.write_all(chunk).await.unwrap();
        socket.write_all(b"\r\n").await.unwrap();
    }
    socket.write_all(b"0\r\n\r\n").await.unwrap();
    let mut response = Vec::new();
    tokio::time::timeout(
        std::time::Duration::from_secs(5),
        socket.read_to_end(&mut response),
    )
    .await
    .unwrap()
    .unwrap();
    assert!(response.starts_with(b"HTTP/1.1 200"), "{response:?}");
    assert!(!String::from_utf8_lossy(&response).contains("error"));
    assert_eq!(
        unary(&client, port, "CloseStdin", json!({"process":{"pid":pid}})).await,
        (200, json!({}))
    );
    output.output(b"first\nsecond\n").await;
    let end = output.next().await.1;
    assert_eq!(end["event"]["end"]["status"], "exit status 0");
    assert_eq!(output.next().await, (2, json!({})));
    assert_eq!(
        unary(&client, port, "List", json!({})).await,
        (200, json!({}))
    );
    server.abort();
}

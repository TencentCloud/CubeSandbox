// SPDX-License-Identifier: Apache-2.0

use base64::{engine::general_purpose::STANDARD, Engine};
use reqwest::Client;
use serde_json::json;
#[path = "support/process.rs"]
mod process;
mod support;
use process::{unary, Stream};

impl Stream {
    async fn output(&mut self, expected: &[u8]) {
        let mut output = Vec::new();
        while output.len() < expected.len() {
            let (flag, frame) = self.next().await;
            assert_eq!(flag, 0, "{frame}");
            let data = frame["event"]["data"].as_object().expect("PTY data");
            assert_eq!(data.len(), 1, "PTY must not emit stdout/stderr");
            output.extend(STANDARD.decode(data["pty"].as_str().unwrap()).unwrap());
        }
        assert_eq!(output, expected);
    }
}

#[tokio::test]
async fn pty_start_accepts_default_zero_and_uint16_wrapped_sizes() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    for pty in [
        json!({}),
        json!({"size":{}}),
        json!({"size":{"rows":0,"cols":80}}),
        json!({"size":{"rows":24,"cols":0}}),
        json!({"size":{"rows":65536,"cols":80}}),
        json!({"size":{"rows":24,"cols":65536}}),
    ] {
        let mut stream = Stream::open(
            &client,
            port,
            "Start",
            json!({
                "process":{"cmd":"/bin/true"},"pty":pty,"tag":"invalid-pty"
            }),
        )
        .await;
        let (flag, first) = stream.next().await;
        assert_eq!(flag, 0, "{first}");
        assert!(first["event"].get("start").is_some());
        assert!(stream.next().await.1["event"].get("end").is_some());
        assert_eq!(stream.next().await, (2, json!({})));
        assert_eq!(
            unary(&client, port, "List", json!({})).await,
            (200, json!({}))
        );
    }
    // Both inclusive uint16 bounds are valid too.
    for size in [1, 65535] {
        let mut stream = Stream::open(
            &client,
            port,
            "Start",
            json!({
                "process":{"cmd":"/bin/true"},
                "pty":{"size":{"rows":size,"cols":size}},"tag":"invalid-pty"
            }),
        )
        .await;
        let (flag, first) = stream.next().await;
        assert_eq!(flag, 0);
        assert!(first["event"].get("start").is_some(), "{first}");
        assert!(stream.next().await.1["event"].get("end").is_some());
        assert_eq!(stream.next().await, (2, json!({})));
    }
    server.abort();
}

#[tokio::test]
async fn pty_reconnect_input_resize_sigwinch_and_terminal_cleanup() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    // Bash defers SIGWINCH traps while an unbounded read is pending.
    let script = "stty -echo; trap 'stty size' WINCH; stty size; while :; do read -t .1 -r line && printf '<%s>\\n' \"$line\"; done";
    let mut owner = Stream::open(
        &client,
        port,
        "Start",
        json!({
            "process":{"cmd":"/bin/bash","args":["-c",script]},
            "pty":{"size":{"rows":24,"cols":80}},"tag":"pty-controls"
        }),
    )
    .await;
    let first = owner.next().await;
    assert_eq!(first.0, 0, "{:?}", first);
    let pid = first.1["event"]["start"]["pid"].clone();
    assert!(pid.is_number(), "{:?}", first);
    owner.output(b"24 80\r\n").await;
    let mut follower = Stream::open(
        &client,
        port,
        "Connect",
        json!({
            "process":{"tag":"pty-controls"}
        }),
    )
    .await;
    assert_eq!(follower.next().await.1["event"]["start"]["pid"], pid);
    drop(owner);
    assert_eq!(
        unary(&client, port, "Update", json!({"process":{"pid":pid}})).await,
        (200, json!({}))
    );
    assert_eq!(
        unary(
            &client,
            port,
            "SendInput",
            json!({
                "process":{"pid":pid},"input":{"stdin":STANDARD.encode(b"wrong\n")}
            })
        )
        .await
        .1["code"],
        "internal"
    );
    assert_eq!(
        unary(
            &client,
            port,
            "CloseStdin",
            json!({
                "process":{"tag":"pty-controls"}
            })
        )
        .await
        .1["code"],
        "unknown"
    );
    assert_eq!(
        unary(
            &client,
            port,
            "SendInput",
            json!({
                "process":{"tag":"pty-controls"},"input":{"pty":STANDARD.encode(b"reconnected\n")}
            })
        )
        .await,
        (200, json!({}))
    );
    follower.output(b"<reconnected>\r\n").await;
    assert_eq!(
        unary(
            &client,
            port,
            "Update",
            json!({
                "process":{"tag":"pty-controls"},"pty":{"size":{"rows":37,"cols":91}}
            })
        )
        .await,
        (200, json!({}))
    );
    // The command reports size only from its SIGWINCH handler after this resize.
    follower.output(b"37 91\r\n").await;
    assert_eq!(
        unary(
            &client,
            port,
            "SendSignal",
            json!({
                "process":{"pid":pid},"signal":9
            })
        )
        .await,
        (200, json!({}))
    );
    assert_eq!(
        follower.next().await.1["event"]["end"]["status"],
        "signal: killed"
    );
    assert_eq!(follower.next().await, (2, json!({})));
    assert_eq!(
        unary(&client, port, "List", json!({})).await,
        (200, json!({}))
    );
    for (method, fields) in [
        ("SendInput", json!({"input":{"pty":""}})),
        ("CloseStdin", json!({})),
        ("Update", json!({})),
        ("SendSignal", json!({"signal":9})),
    ] {
        let mut request = fields;
        request["process"] = json!({"tag":"pty-controls"});
        assert_eq!(
            unary(&client, port, method, request).await.1["code"],
            "not_found"
        );
    }
    let mut missing = Stream::open(&client, port, "Connect", json!({"process":{"pid":pid}})).await;
    assert_eq!(missing.next().await.1["error"]["code"], "not_found");
    let mut replacement = Stream::open(
        &client,
        port,
        "Start",
        json!({
            "process":{"cmd":"/bin/sleep","args":["30"]},"tag":"pty-controls"
        }),
    )
    .await;
    let pid = replacement.next().await.1["event"]["start"]["pid"].clone();
    assert_eq!(
        unary(
            &client,
            port,
            "Update",
            json!({
                "process":{"pid":pid},"pty":{"size":{"rows":24,"cols":80}}
            })
        )
        .await
        .1["code"],
        "internal"
    );
    assert_eq!(
        unary(
            &client,
            port,
            "SendInput",
            json!({
                "process":{"pid":pid},"input":{"pty":STANDARD.encode(b"wrong\n")}
            })
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
            json!({
                "process":{"pid":pid},"signal":9
            })
        )
        .await,
        (200, json!({}))
    );
    assert!(replacement.next().await.1["event"].get("end").is_some());
    assert_eq!(replacement.next().await, (2, json!({})));
    server.abort();
}

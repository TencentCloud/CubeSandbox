// SPDX-License-Identifier: Apache-2.0

use base64::{engine::general_purpose::STANDARD, Engine};
use reqwest::Client;
use serde_json::{json, Value};
use std::time::Duration;
use tokio::task::JoinHandle;
#[path = "support/process.rs"]
mod process;
mod support;
use process::{unary as rpc, Stream};

impl Stream {
    async fn marker(&mut self, expected: &[u8]) {
        let mut output = vec![];
        while output.len() < expected.len() {
            let (flags, event) = self.next_with_timeout(6).await;
            assert_eq!(flags, 0, "{event}");
            output.extend(
                STANDARD
                    .decode(
                        event["event"]["data"]["stdout"]
                            .as_str()
                            .expect("stdout marker"),
                    )
                    .unwrap(),
            );
        }
        assert_eq!(output, expected);
    }
    async fn finish(&mut self) -> Vec<u8> {
        self.finish_with_status().await.0
    }
    async fn finish_with_status(&mut self) -> (Vec<u8>, Vec<u8>, Value) {
        let mut output = vec![];
        let mut stderr = vec![];
        let end = loop {
            let (flags, event) = self.next_with_timeout(6).await;
            assert_eq!(flags, 0, "{event}");
            if let Some(end) = event["event"].get("end") {
                break end.clone();
            }
            if let Some(data) = event["event"]["data"]["stdout"].as_str() {
                output.extend(STANDARD.decode(data).unwrap());
            }
            if let Some(data) = event["event"]["data"]["stderr"].as_str() {
                stderr.extend(STANDARD.decode(data).unwrap());
            }
        };
        assert_eq!(self.next_with_timeout(6).await, (2, json!({})));
        (output, stderr, end)
    }
}
fn write(client: &Client, port: u16, byte: u8, size: usize) -> JoinHandle<(u16, Value)> {
    let client = client.clone();
    tokio::spawn(async move {
        rpc(
            &client,
            port,
            "SendInput",
            json!({"process":{"tag":"pressure"},
        "input":{"stdin":STANDARD.encode(vec![byte;size])}}),
        )
        .await
    })
}
async fn start(client: &Client, port: u16, script: &str, deadline: u64) -> Stream {
    let mut stream = Stream::timed(
        client,
        port,
        "Start",
        json!({"tag":"pressure","stdin":true,
        "process":{"cmd":"/usr/bin/python3","args":["-u","-c",script]}}),
        false,
        (deadline > 0).then(|| deadline.to_string()).as_deref(),
    )
    .await;
    let first = stream.next_with_timeout(6).await;
    assert!(first.1["event"]["start"]["pid"].is_number(), "{first:?}");
    stream
}
async fn cleaned(client: &Client, port: u16) {
    assert_eq!(rpc(client, port, "List", json!({})).await, (200, json!({})));
    for method in ["SendInput", "CloseStdin", "SendSignal"] {
        let mut request = json!({"process":{"tag":"pressure"}});
        if method == "SendSignal" {
            request["signal"] = json!(9);
        }
        if method == "SendInput" {
            request["input"] = json!({"stdin":""});
        }
        let (_, error) = rpc(client, port, method, request).await;
        assert_eq!(error["code"], "not_found", "{method}: {error}");
    }
    let mut reused = start(client, port, "print('reused')", 0).await;
    assert_eq!(reused.finish().await, b"reused\n");
}

#[tokio::test]
async fn concurrent_payloads_are_serial_and_close_follows_accepted_writes() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    let directory = tempfile::tempdir().unwrap();
    let release = directory.path().join("release");
    let script=format!("import os,time,itertools,json\nwhile not os.path.exists({:?}): time.sleep(.01)\ndata=b''\nwhile True:\n chunk=os.read(0,65536)\n if not chunk: break\n data+=chunk\nprint(json.dumps([(k,len(list(v))) for k,v in itertools.groupby(data)]))",release);
    let mut stream = start(&client, port, &script, 5000).await;
    let writes: Vec<_> = (b'A'..=b'C')
        .map(|byte| write(&client, port, byte, 262144))
        .collect();
    tokio::time::sleep(Duration::from_millis(500)).await;
    assert!(
        writes.iter().all(|request| !request.is_finished()),
        "full kernel writes must block before reader starts"
    );
    let close_client = client.clone();
    let close = tokio::spawn(async move {
        rpc(
            &close_client,
            port,
            "CloseStdin",
            json!({"process":{"tag":"pressure"}}),
        )
        .await
    });
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(
        !close.is_finished(),
        "CloseStdin must queue after accepted writes"
    );
    std::fs::write(release, b"").unwrap();
    for request in writes {
        assert_eq!(request.await.unwrap(), (200, json!({})));
    }
    assert_eq!(close.await.unwrap(), (200, json!({})));
    let mut runs: Vec<(u8, usize)> = serde_json::from_slice(&stream.finish().await).unwrap();
    runs.sort_unstable();
    assert_eq!(runs, vec![(b'A', 262144), (b'B', 262144), (b'C', 262144)]);
    cleaned(&client, port).await;
    server.abort();
}

#[tokio::test]
async fn saturated_writer_does_not_delay_kill_or_deadline_and_subscribers_end() {
    for deadline in [0, 3000] {
        let (port, server) = support::spawn_daemon().await;
        let client = Client::builder().no_proxy().build().unwrap();
        let mut stream = start(&client, port, "import time; time.sleep(30)", deadline).await;
        let mut follower = Stream::timed(
            &client,
            port,
            "Connect",
            json!({"process":{"tag":"pressure"}}),
            false,
            None,
        )
        .await;
        assert!(follower.next_with_timeout(6).await.1["event"]["start"]["pid"].is_number());
        let writes: Vec<_> = (0..32)
            .map(|_| write(&client, port, b'X', 262144))
            .collect();
        tokio::time::sleep(Duration::from_millis(500)).await;
        assert!(
            writes.iter().all(|write| !write.is_finished()),
            "blocked requests must wait without queue-capacity rejection"
        );
        // Empty input is ordered behind writes by the oracle, not an immediate
        // no-op. Its public blocked-writer behavior is covered by remaining_compat.
        if deadline == 0 {
            assert_eq!(
                tokio::time::timeout(
                    Duration::from_millis(500),
                    rpc(
                        &client,
                        port,
                        "SendSignal",
                        json!({"process":{"tag":"pressure"},"signal":9})
                    )
                )
                .await
                .expect("signal bypasses writer"),
                (200, json!({}))
            );
        }
        if deadline == 0 {
            assert!(stream.finish().await.is_empty());
        } else {
            assert_eq!(
                stream.next_with_timeout(6).await.1["error"]["code"],
                "deadline_exceeded"
            );
        }
        assert!(follower.finish().await.is_empty());
        for request in writes {
            let (status, error) = tokio::time::timeout(Duration::from_secs(1), request)
                .await
                .expect("wait cancels writer")
                .unwrap();
            assert_ne!(status, 200, "blocked input cannot acknowledge full write");
            // A closed pipe can be observed before wait removes the selector.
            // Queued requests that wrote no bytes then report a known closed
            // input. Partial delivery remains unknown; cancellation may also
            // leave the caller unable to determine whether delivery started.
            match error["code"].as_str().unwrap() {
                "not_found" => {}
                "internal" => {
                    assert!(
                        error["message"].as_str().unwrap().contains("closed")
                            || error["message"].as_str().unwrap().contains("broken pipe"),
                        "{error}"
                    );
                }
                "unknown" => {
                    assert!(
                        error["message"].as_str().unwrap().contains("prefix"),
                        "{error}"
                    );
                }
                _ => panic!("unexpected blocked-input result: {error}"),
            }
        }
        cleaned(&client, port).await;
        server.abort();
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn queued_inputs_all_complete_and_term_does_not_cancel_writer() {
    for ignored in [false, true] {
        let (port, server) = support::spawn_daemon().await;
        let client = Client::builder().no_proxy().build().unwrap();
        let directory = tempfile::tempdir().unwrap();
        let release = directory.path().join("release");
        let handler = if ignored {
            "signal.SIG_IGN"
        } else {
            "lambda *_: print('caught',flush=True)"
        };
        let script=format!("import os,time,signal,itertools,json\nsignal.signal(signal.SIGTERM,{handler})\nprint('ready')\ndata=os.read(0,1)\nprint('prefix-read')\nwhile not os.path.exists({release:?}): time.sleep(.01)\nwhile True:\n chunk=os.read(0,65536)\n if not chunk: break\n data+=chunk\nprint(json.dumps([(k,len(list(v))) for k,v in itertools.groupby(data)]))");
        let mut stream = start(&client, port, &script, 10000).await;
        stream.marker(b"ready\n").await;
        // The real pipe read proves this request was admitted before TERM.
        let admitted = write(&client, port, b'A', 262144);
        stream.marker(b"prefix-read\n").await;
        // Distinct payloads prove all waiting writes are delivered intact.
        let pending: Vec<_> = (b'B'..=b'Z')
            .map(|byte| (byte, write(&client, port, byte, 262144)))
            .collect();
        tokio::time::sleep(Duration::from_millis(500)).await;
        assert!(pending.iter().all(|(_, request)| !request.is_finished()));
        assert_eq!(
            tokio::time::timeout(
                Duration::from_millis(500),
                rpc(
                    &client,
                    port,
                    "SendSignal",
                    json!({"process":{"tag":"pressure"},"signal":15})
                )
            )
            .await
            .expect("TERM bypasses input"),
            (200, json!({}))
        );
        if !ignored {
            stream.marker(b"caught\n").await;
        }
        assert!(
            !admitted.is_finished(),
            "successful signal delivery must not cancel writer"
        );
        std::fs::write(release, b"").unwrap();
        assert_eq!(admitted.await.unwrap(), (200, json!({})));
        let mut expected = vec![(b'A', 262144)];
        for (byte, request) in pending {
            assert_eq!(request.await.unwrap(), (200, json!({})));
            expected.push((byte, 262144));
        }
        // Control remains usable after caught or ignored TERM.
        assert_eq!(
            write(&client, port, b'!', 17).await.unwrap(),
            (200, json!({}))
        );
        expected.push((b'!', 17));
        assert_eq!(
            rpc(
                &client,
                port,
                "CloseStdin",
                json!({"process":{"tag":"pressure"}})
            )
            .await,
            (200, json!({}))
        );
        let (output, stderr, end) = stream.finish_with_status().await;
        let stderr = String::from_utf8_lossy(&stderr);
        assert!(
            end["exited"] == true && end["exitCode"].as_i64().unwrap_or(0) == 0,
            "reader failed (ignored TERM: {ignored}): end={end}, stderr={stderr}"
        );
        let mut received: Vec<(u8, usize)> = serde_json::from_slice(&output).unwrap_or_else(|error| {
            panic!(
                "invalid reader output (ignored TERM: {ignored}): {error}; end={end}, stderr={stderr}, stdout={}",
                String::from_utf8_lossy(&output)
            )
        });
        received.sort_unstable();
        expected.sort_unstable();
        assert_eq!(
            received, expected,
            "all serialized payloads are delivered without interleaving"
        );
        cleaned(&client, port).await;
        server.abort();
    }
}

#[tokio::test]
async fn expired_request_deadline_preserves_accepted_write_and_later_input() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    let directory = tempfile::tempdir().unwrap();
    let release = directory.path().join("release");
    let script=format!("import os,time,json\nfirst=os.read(0,1)\nprint('prefix-read')\nwhile not os.path.exists({release:?}): time.sleep(.01)\ndata=first\nwhile True:\n chunk=os.read(0,65536)\n if not chunk: break\n data+=chunk\nprint(json.dumps([len(data),data.count(b'A'),data.count(b'Z'),data==b'A'*data.count(b'A')+b'Z']))");
    let mut stream = start(&client, port, &script, 5000).await;
    let send_client = client.clone();
    let mut request = tokio::spawn(async move {
        send_client.post(format!("http://127.0.0.1:{port}/process.Process/SendInput"))
            .header("connect-protocol-version","1").header("connect-timeout-ms","500")
            .json(&json!({"process":{"tag":"pressure"},"input":{"stdin":STANDARD.encode(vec![b'A';1024*1024])}}))
            .send().await.unwrap().json::<Value>().await.unwrap()
    });
    stream.marker(b"prefix-read\n").await;
    assert!(
        tokio::time::timeout(Duration::from_millis(700), &mut request)
            .await
            .is_err()
    );
    std::fs::write(release, b"").unwrap();
    assert_eq!(request.await.unwrap(), json!({}));
    assert_eq!(
        write(&client, port, b'Z', 1).await.unwrap(),
        (200, json!({}))
    );
    assert_eq!(
        rpc(
            &client,
            port,
            "CloseStdin",
            json!({"process":{"tag":"pressure"}})
        )
        .await,
        (200, json!({}))
    );
    let result: Value = serde_json::from_slice(&stream.finish().await).unwrap();
    assert_eq!(
        result[1],
        1024 * 1024,
        "accepted write must finish: {result}"
    );
    assert_eq!(result[2], 1);
    assert_eq!(result[3], true);
    cleaned(&client, port).await;
    server.abort();
}

#[tokio::test]
async fn partial_kernel_error_reports_internal_before_terminal() {
    let (port, server) = support::spawn_daemon().await;
    let client = Client::builder().no_proxy().build().unwrap();
    let mut stream = start(
        &client,
        port,
        "import os,time; data=os.read(0,1); os.close(0); print(data.decode()); time.sleep(1)",
        5000,
    )
    .await;
    let (status, error) = write(&client, port, b'P', 1024 * 1024).await.unwrap();
    assert_ne!(status, 200);
    assert_eq!(error["code"], "internal", "{error}");
    assert!(
        error["message"].as_str().unwrap().contains("broken pipe"),
        "{error}"
    );
    assert!(
        !rpc(&client, port, "List", json!({})).await.1["processes"]
            .as_array()
            .unwrap()
            .is_empty(),
        "pipe failure is not terminal"
    );
    assert_eq!(stream.finish().await, b"P\n");
    cleaned(&client, port).await;
    server.abort();
}

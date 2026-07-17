// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::process::{Child, Command, Stdio};

struct Guard(Child);

impl Drop for Guard {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[test]
fn binary_serves_health_on_the_configured_address() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let mut child = Guard(
        Command::new(env!("CARGO_BIN_EXE_crash-recovery-worker"))
            .env("WORKER_ADDR", "127.0.0.1:0")
            .env("AUDIT_PATH", dir.path().join("audit.jsonl"))
            .stdout(Stdio::piped())
            .spawn()
            .expect("start worker"),
    );
    let stdout = child.0.stdout.take().expect("capture worker stdout");
    let mut lines = BufReader::new(stdout).lines();
    let listening = lines
        .next()
        .expect("worker startup line")
        .expect("read worker startup line");
    let address = listening
        .strip_prefix("listening=http://")
        .expect("parse worker address");

    let mut stream = TcpStream::connect(address).expect("connect to worker");
    stream
        .write_all(b"GET /health HTTP/1.1\r\nHost: worker\r\nConnection: close\r\n\r\n")
        .expect("send health request");

    let mut response = String::new();
    stream
        .read_to_string(&mut response)
        .expect("read health response");

    assert!(response.starts_with("HTTP/1.1 200 OK"), "{response}");
    assert!(response.ends_with(r#"{"status":"ok"}"#), "{response}");
}

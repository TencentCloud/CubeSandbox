// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// End-to-end guard for the rate-limiter bucket-key bypass (#1379).
//
// Builds and launches the real cube-api binary, points it at a stub CubeMaster,
// and drives it over HTTP the way the original probe did. Ignored by default so
// the unit gate stays fast; run with:
//
//     cd CubeAPI && cargo test --test rate_limit_e2e -- --ignored --test-threads=1

use std::io::{BufRead, BufReader};
use std::net::TcpListener;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

const API_KEY: &str = "supersecret";
const RATE_LIMIT_PER_SEC: &str = "3";

fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .expect("reserve a port")
        .local_addr()
        .expect("local addr")
        .port()
}

/// A stub CubeMaster that accepts connections and answers every request with a
/// minimal envelope, so cube-api reaches its handlers instead of erroring early.
fn spawn_stub_cubemaster() -> u16 {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind stub cubemaster");
    let port = listener.local_addr().expect("addr").port();
    std::thread::spawn(move || {
        for stream in listener.incoming() {
            let Ok(mut stream) = stream else { continue };
            std::thread::spawn(move || {
                use std::io::Write;
                let mut reader = BufReader::new(stream.try_clone().expect("clone"));
                let mut line = String::new();
                // Drain the request head; we do not care about its contents.
                while reader.read_line(&mut line).unwrap_or(0) > 0 {
                    if line == "\r\n" || line == "\n" {
                        break;
                    }
                    line.clear();
                }
                let body = r#"{"requestID":"stub","ret":{"ret_code":0,"ret_msg":"ok"},"data":{}}"#;
                let _ = write!(
                    stream,
                    "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                    body.len(),
                    body
                );
            });
        }
    });
    port
}

struct Server {
    child: Child,
    base_url: String,
}

impl Drop for Server {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn binary_path() -> std::path::PathBuf {
    // integration tests live next to the built binary's target dir
    let mut p = std::env::current_exe().expect("current exe");
    p.pop(); // deps/
    p.pop(); // debug/
    p.push("cube-api");
    p
}

fn start_server() -> Server {
    let binary = binary_path();
    if !binary.exists() {
        let status = Command::new(env!("CARGO"))
            .args(["build", "--bin", "cube-api"])
            .status()
            .expect("build cube-api");
        assert!(status.success(), "building cube-api failed");
    }

    let port = free_port();
    let cubemaster_port = spawn_stub_cubemaster();

    let child = Command::new(&binary)
        .args(["--bind", &format!("127.0.0.1:{port}")])
        .args(["--rate-limit-per-sec", RATE_LIMIT_PER_SEC])
        .env("CUBE_API_KEY", API_KEY)
        .env(
            "CUBE_MASTER_ADDR",
            format!("http://127.0.0.1:{cubemaster_port}"),
        )
        .env("RUST_LOG", "warn")
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn cube-api");

    let base_url = format!("http://127.0.0.1:{port}");
    let deadline = Instant::now() + Duration::from_secs(60);
    while Instant::now() < deadline {
        if TcpListener::bind(("127.0.0.1", port)).is_err() {
            // port is taken => the server is listening
            return Server { child, base_url };
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    panic!("cube-api never started listening on {base_url}");
}

/// Minimal blocking HTTP GET returning the status code, so the test needs no
/// extra dependency.
fn get(base_url: &str, path: &str, headers: &[(&str, String)]) -> u16 {
    use std::io::{Read, Write};
    let addr = base_url.trim_start_matches("http://");
    let mut stream = std::net::TcpStream::connect(addr).expect("connect to cube-api");
    stream
        .set_read_timeout(Some(Duration::from_secs(10)))
        .expect("read timeout");

    let mut req = format!("GET {path} HTTP/1.1\r\nHost: {addr}\r\nConnection: close\r\n");
    for (k, v) in headers {
        req.push_str(&format!("{k}: {v}\r\n"));
    }
    req.push_str("\r\n");
    stream.write_all(req.as_bytes()).expect("write request");

    let mut raw = Vec::new();
    let _ = stream.read_to_end(&mut raw);
    let head = String::from_utf8_lossy(&raw);
    head.lines()
        .next()
        .and_then(|l| l.split_whitespace().nth(1))
        .and_then(|c| c.parse().ok())
        .unwrap_or(0)
}

fn count_throttled(
    server: &Server,
    requests: usize,
    headers: impl Fn(usize) -> Vec<(&'static str, String)>,
) -> usize {
    (0..requests)
        .filter(|i| get(&server.base_url, "/sandboxes", &headers(*i)) == 429)
        .count()
}

#[test]
#[ignore = "spawns the real cube-api binary; run with --ignored"]
fn rotating_an_unvalidated_api_key_header_cannot_refresh_the_bucket() {
    let server = start_server();

    let throttled = count_throttled(&server, 30, |i| {
        vec![
            ("Authorization", format!("Bearer {API_KEY}")),
            ("X-API-Key", format!("rotating-{i}")),
        ]
    });

    assert!(
        throttled > 20,
        "rotating X-API-Key bypassed the limiter over the wire: only {throttled}/30 throttled"
    );
}

#[test]
#[ignore = "spawns the real cube-api binary; run with --ignored"]
fn a_bearer_client_is_throttled_end_to_end() {
    let server = start_server();

    let throttled = count_throttled(&server, 30, |_| {
        vec![("Authorization", format!("Bearer {API_KEY}"))]
    });

    assert!(
        throttled > 20,
        "a plain Bearer client was not throttled over the wire: {throttled}/30"
    );
}

#[test]
#[ignore = "spawns the real cube-api binary; run with --ignored"]
fn an_invalid_credential_is_rejected_end_to_end() {
    let server = start_server();

    let code = get(
        &server.base_url,
        "/sandboxes",
        &[("Authorization", "Bearer wrong-key".to_string())],
    );
    assert_eq!(code, 401, "an invalid credential should be rejected");
}

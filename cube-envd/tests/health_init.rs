// SPDX-License-Identifier: Apache-2.0

use std::convert::Infallible;

use axum::body::Body;
use axum::http::Request;
use reqwest::StatusCode;
use serde_json::Value;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpStream;
use tokio::time::{sleep, timeout, Duration};
use tower::ServiceExt;

use cube_envd::server::{run_server, ServerConfig, ServerPhase};

mod support;

async fn free_port() -> u16 {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    listener.local_addr().expect("addr").port()
}

#[tokio::test]
async fn health_is_ready_before_init() {
    let (port, lifecycle, handle) = support::spawn_envd(ServerPhase::Ready).await;
    assert!(lifecycle.is_ready());

    let response = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/health"))
        .send()
        .await
        .expect("health");
    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    assert!(!response.headers().contains_key("x-request-id"));
    assert_eq!(
        response
            .headers()
            .get("cache-control")
            .and_then(|value| value.to_str().ok()),
        Some("no-store")
    );
    assert!(response.bytes().await.expect("body").is_empty());
    handle.abort();
}

#[tokio::test]
async fn production_server_stays_ready_while_waiting_for_shutdown() {
    let port = free_port().await;
    let server = tokio::spawn(run_server(ServerConfig {
        port,
        is_not_fc: true,
    }));
    let client = reqwest::Client::new();
    let mut status = None;
    for _ in 0..20 {
        if let Ok(response) = client
            .get(format!("http://127.0.0.1:{port}/health"))
            .send()
            .await
        {
            status = Some(response.status());
            break;
        }
        sleep(Duration::from_millis(10)).await;
    }
    assert_eq!(status, Some(StatusCode::NO_CONTENT));
    server.abort();
}

#[tokio::test]
async fn health_non_ready_is_503() {
    let (port, _lifecycle, server) = support::spawn_envd(ServerPhase::Booting).await;

    let response = reqwest::Client::new()
        .get(format!("http://127.0.0.1:{port}/health"))
        .send()
        .await
        .expect("health");
    assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
    server.abort();
}

#[tokio::test]
async fn init_post_empty_and_body_limit_contract() {
    let (port, _lifecycle, handle) = support::spawn_envd(ServerPhase::Ready).await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/init");

    let ordinary = client
        .post(&url)
        .header("x-access-token", "ignored")
        .body("{}")
        .send()
        .await
        .expect("init");
    assert_eq!(ordinary.status(), StatusCode::NO_CONTENT);
    assert_eq!(ordinary.headers().get("cache-control").unwrap(), "no-store");
    assert_eq!(ordinary.headers()["content-type"], "");
    assert!(ordinary.bytes().await.expect("ordinary body").is_empty());

    let empty = client.post(&url).send().await.expect("empty init");
    assert_eq!(empty.status(), StatusCode::NO_CONTENT);
    assert_eq!(empty.headers().get("cache-control").unwrap(), "no-store");
    assert!(empty.bytes().await.expect("empty body").is_empty());

    let oversize = client
        .post(&url)
        .body("x".repeat(cube_envd::transport::limits::INIT_BODY_LIMIT + 1))
        .send()
        .await
        .expect("oversize init");
    assert_eq!(oversize.status(), StatusCode::PAYLOAD_TOO_LARGE);
    assert_eq!(oversize.headers().get("cache-control").unwrap(), "no-store");
    assert_eq!(
        oversize.headers().get("content-type").unwrap(),
        "text/plain; charset=utf-8"
    );

    let mut stream = TcpStream::connect(("127.0.0.1", port))
        .await
        .expect("raw init connection");
    let advertised = cube_envd::transport::limits::INIT_BODY_LIMIT + 2;
    let headers = format!(
        "POST /init HTTP/1.1\r\nHost: localhost\r\nContent-Length: {advertised}\r\nConnection: close\r\n\r\n"
    );
    stream
        .write_all(headers.as_bytes())
        .await
        .expect("write raw init headers");
    stream
        .write_all(&vec![b'x'; advertised - 1])
        .await
        .expect("write enough bytes to exceed cap");
    let mut reader = BufReader::new(stream);
    let mut status_line = String::new();
    timeout(Duration::from_secs(1), reader.read_line(&mut status_line))
        .await
        .expect("413 before complete buffering")
        .expect("read raw init status");
    assert!(status_line.contains(" 413 "), "status={status_line:?}");

    let wrong_method = client.get(&url).send().await.expect("GET init");
    assert_eq!(wrong_method.status(), StatusCode::METHOD_NOT_ALLOWED);
    assert_eq!(wrong_method.headers()["allow"], "POST");
    handle.abort();
}

#[tokio::test]
async fn init_rejects_malformed_non_object_and_excessive_depth() {
    let (port, _lifecycle, handle) = support::spawn_envd(ServerPhase::Ready).await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/init");

    let cases = [("{", ""), ("[]", "")];
    for (body, expected) in cases {
        let response = client
            .post(&url)
            .body(body)
            .send()
            .await
            .expect("invalid init");
        assert_eq!(response.status(), StatusCode::BAD_REQUEST, "body={body}");
        assert_eq!(response.text().await.expect("error body"), expected);
    }

    let too_deep = format!(
        "{{\"unknown\":{}{}}}",
        "[".repeat(cube_envd::transport::limits::INIT_JSON_DEPTH_LIMIT),
        "]".repeat(cube_envd::transport::limits::INIT_JSON_DEPTH_LIMIT)
    );
    let response = client
        .post(&url)
        .body(too_deep)
        .send()
        .await
        .expect("deep init");
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    assert_eq!(
        response.text().await.expect("depth error"),
        "JSON nesting exceeds limit"
    );
    handle.abort();
}

#[tokio::test]
async fn init_validates_known_wire_types_but_ignores_unknown_fields() {
    let (port, _lifecycle, handle) = support::spawn_envd(ServerPhase::Ready).await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/init");

    for body in [
        r#"{"envVars":[]}"#,
        r#"{"envVars":{"KEY":1}}"#,
        r#"{"defaultUser":1}"#,
        r#"{"defaultWorkdir":false}"#,
        r#"{"timestamp":1}"#,
        r#"{"accessToken":[]}"#,
        r#"{"caBundle":[]}"#,
        r#"{"hyperloopIP":[]}"#,
        r#"{"volumeMounts":{}}"#,
    ] {
        let response = client
            .post(&url)
            .body(body)
            .send()
            .await
            .expect("known wire type");
        assert_eq!(response.status(), StatusCode::BAD_REQUEST, "body={body}");
        assert_eq!(response.text().await.expect("wire error"), "");
    }

    let ignored = client
        .post(&url)
        .body(r#"{"unknown":{"any":[true,1,"value"]},"unknown":null}"#)
        .send()
        .await
        .expect("unknown fields");
    assert_eq!(ignored.status(), StatusCode::NO_CONTENT);
    handle.abort();
}

#[tokio::test]
async fn init_treats_gzip_bytes_as_malformed_raw_json() {
    const GZIP_EMPTY_OBJECT: &[u8] = &[
        0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xab, 0xae, 0x05, 0x00, 0x43,
        0xbf, 0xa6, 0xa3, 0x02, 0x00, 0x00, 0x00,
    ];
    let (port, _lifecycle, handle) = support::spawn_envd(ServerPhase::Ready).await;
    let response = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/init"))
        .header("content-encoding", "gzip")
        .body(GZIP_EMPTY_OBJECT)
        .send()
        .await
        .expect("gzip init");
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    assert_eq!(response.text().await.expect("gzip error"), "");
    handle.abort();
}

#[tokio::test]
async fn init_rejects_every_non_ready_lifecycle_phase() {
    for phase in [
        ServerPhase::Booting,
        ServerPhase::Listening,
        ServerPhase::Draining,
        ServerPhase::Failed,
        ServerPhase::Stopped,
    ] {
        let (port, state, handle) = support::spawn_envd_with_state(phase).await;
        let response = reqwest::Client::new()
            .post(format!("http://127.0.0.1:{port}/init"))
            .body(r#"{"envVars":{"SHOULD_NOT":"COMMIT"}}"#)
            .send()
            .await
            .expect("init");
        assert_eq!(
            response.status(),
            StatusCode::SERVICE_UNAVAILABLE,
            "{phase:?}"
        );
        assert_eq!(state.runtime.snapshot().await.generation(), 0);
        assert_eq!(
            state
                .runtime
                .snapshot()
                .await
                .environment()
                .get("SHOULD_NOT"),
            None
        );
        handle.abort();
    }
}

#[tokio::test]
async fn admitted_init_finishes_after_draining_but_new_init_is_rejected() {
    let (_port, state, handle) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let (body_polled, body_poll) = tokio::sync::oneshot::channel();
    let (release_body, body_release) = tokio::sync::oneshot::channel();
    let body = Body::from_stream(futures::stream::once(async move {
        body_polled.send(()).expect("signal body poll");
        body_release.await.expect("release body");
        Ok::<_, Infallible>(r#"{"envVars":{"ADMITTED":"yes"}}"#)
    }));
    let request = Request::post("/init").body(body).expect("init request");
    let admitted = tokio::spawn(cube_envd::transport::build_router(state.clone()).oneshot(request));

    body_poll.await.expect("request admitted while Ready");
    assert!(state.lifecycle.try_transition(ServerPhase::Draining));
    release_body.send(()).expect("finish admitted request");

    let response = admitted
        .await
        .expect("admitted request task")
        .expect("init");
    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    assert_eq!(state.runtime.snapshot().await.generation(), 1);
    assert_eq!(
        state
            .runtime
            .snapshot()
            .await
            .environment()
            .get("ADMITTED")
            .map(String::as_str),
        Some("yes")
    );

    let response = cube_envd::transport::build_router(state.clone())
        .oneshot(
            Request::post("/init")
                .body(Body::from(r#"{"envVars":{"NEW":"no"}}"#))
                .expect("new init request"),
        )
        .await
        .expect("new init");
    assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
    let snapshot = state.runtime.snapshot().await;
    assert_eq!(snapshot.generation(), 1);
    assert_eq!(snapshot.environment().get("NEW"), None);
    handle.abort();
}

#[tokio::test]
async fn default_user_040_threshold_fixture() {
    let fixture: Value = serde_json::from_str(include_str!(
        "../contracts/fixtures/default-user-0.4.0.json"
    ))
    .expect("default-user fixture");
    assert_eq!(fixture["feature_gate"], "0.4.0");

    let (port, _state, handle) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = reqwest::Client::new();
    for case in fixture["cases"].as_array().expect("fixture cases") {
        let response = client
            .post(format!("http://127.0.0.1:{port}/init"))
            .body(case["request"].to_string())
            .send()
            .await
            .expect("default-user fixture request");
        assert_eq!(
            response.status().as_u16(),
            case["status"].as_u64().expect("fixture status") as u16,
            "case={}",
            case["name"]
        );
    }
    handle.abort();
}

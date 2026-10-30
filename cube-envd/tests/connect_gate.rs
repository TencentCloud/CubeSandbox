// SPDX-License-Identifier: Apache-2.0

use std::sync::Arc;

use connectrpc::Router as ConnectRouter;
use reqwest::StatusCode;
use serde_json::{json, Value};

fn encode_envelope(flags: u8, value: Value) -> Vec<u8> {
    let payload = serde_json::to_vec(&value).expect("json");
    encode_raw_envelope(flags, &payload)
}

fn encode_raw_envelope(flags: u8, payload: &[u8]) -> Vec<u8> {
    let mut envelope = Vec::with_capacity(payload.len() + 5);
    envelope.push(flags);
    envelope.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    envelope.extend_from_slice(payload);
    envelope
}

fn decode_envelopes(mut body: &[u8]) -> Vec<(u8, Value)> {
    let mut envelopes = Vec::new();
    while !body.is_empty() {
        assert!(body.len() >= 5, "truncated envelope header");
        let flags = body[0];
        let len = u32::from_be_bytes(body[1..5].try_into().expect("length")) as usize;
        assert!(body.len() >= len + 5, "truncated envelope payload");
        let payload = serde_json::from_slice(&body[5..5 + len]).expect("envelope json");
        envelopes.push((flags, payload));
        body = &body[5 + len..];
    }
    envelopes
}

async fn spawn_gate_server() -> (u16, tokio::task::JoinHandle<()>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    let router = cube_envd::conformance::build_gate_router();
    let handle = tokio::spawn(async move {
        axum::serve(listener, router).await.expect("serve");
    });
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    (port, handle)
}

async fn spawn_limited_gate_server(
    limits: connectrpc::Limits,
) -> (u16, tokio::task::JoinHandle<()>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    let connect =
        ConnectRouter::new().add_service(Arc::new(cube_envd::conformance::echo::EchoGateService));
    let router = axum::Router::new().fallback_service(
        connect
            .into_axum_service()
            .with_limits(limits)
            .with_interceptor(cube_envd::transport::limits::BudgetErrorInterceptor),
    );
    let handle = tokio::spawn(async move {
        axum::serve(listener, router).await.expect("serve");
    });
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    (port, handle)
}

#[test]
fn production_connect_accepts_upstream_message_sizes() {
    let limits = cube_envd::transport::limits::connect_limits();
    assert_eq!(limits.max_request_body_size(), usize::MAX);
    assert_eq!(limits.max_message_size(), usize::MAX);
    assert_eq!(limits.element_memory_limit(), usize::MAX);
}

#[tokio::test]
async fn connect_json_unary_roundtrip() {
    let (port, handle) = spawn_gate_server().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/conformance.echo.Echo/Unary");
    let resp = client
        .post(&url)
        .header("content-type", "application/json")
        .header("connect-protocol-version", "1")
        .body(
            r#"{"message":"hello","bigNumber":"9223372036854775807","kind":"ECHO_ENUM_ALPHA","payload":"aGVsbG8=","name":"alice"}"#,
        )
        .send()
        .await
        .expect("post");
    assert_eq!(resp.status(), StatusCode::OK);
    let body: Value = resp.json().await.expect("body");
    assert_eq!(body["message"], "hello");
    assert_eq!(body["bigNumber"], "9223372036854775807");
    assert_eq!(body["kind"], "ECHO_ENUM_ALPHA");
    assert_eq!(body["payload"], "aGVsbG8=");
    assert_eq!(body["selectedName"], "alice");
    handle.abort();
}

#[tokio::test]
async fn connect_json_unary_error_mapping() {
    let (port, handle) = spawn_gate_server().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/conformance.echo.Echo/Unary");
    let resp = client
        .post(&url)
        .header("content-type", "application/json")
        .header("connect-protocol-version", "1")
        .body(r#"{"message":"error"}"#)
        .send()
        .await
        .expect("post");
    assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
    handle.abort();
}

#[tokio::test]
async fn malformed_unary_json_is_bounded_invalid_argument() {
    let (port, handle) = spawn_gate_server().await;
    let response = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/conformance.echo.Echo/Unary"
        ))
        .header("content-type", "application/json")
        .header("connect-protocol-version", "1")
        .body("{")
        .send()
        .await
        .expect("post");
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    let error: Value = response.json().await.expect("Connect error JSON");
    assert_eq!(error["code"], "invalid_argument");
    handle.abort();
}

#[tokio::test]
async fn connect_timeout_header_matrix() {
    let (port, handle) = spawn_gate_server().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/conformance.echo.Echo/Unary");

    for (header, expect_ok) in [
        (None, true),
        (Some("0"), true),
        (Some("-1"), true),
        (Some("1000"), true),
        (Some(""), true),
        (Some("nope"), false),
    ] {
        let mut req = client
            .post(&url)
            .header("content-type", "application/json")
            .header("connect-protocol-version", "1")
            .body(r#"{"message":"t"}"#);
        if let Some(v) = header {
            req = req.header("connect-timeout-ms", v);
        }
        let resp = req.send().await.expect("post");
        if expect_ok {
            assert_eq!(resp.status(), StatusCode::OK, "header={header:?}");
        } else {
            assert_eq!(resp.status(), StatusCode::BAD_REQUEST, "header={header:?}");
        }
    }
    handle.abort();
}

#[tokio::test]
async fn connect_json_server_stream_emits_start_data_end() {
    let (port, handle) = spawn_gate_server().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/conformance.echo.Echo/ServerStream");
    let resp = client
        .post(&url)
        .header("content-type", "application/connect+json")
        .header("connect-protocol-version", "1")
        .body(encode_envelope(
            0,
            json!({"message": "stream", "payload": "AQID"}),
        ))
        .send()
        .await
        .expect("post");
    assert_eq!(resp.status(), StatusCode::OK);
    let bytes = resp.bytes().await.expect("bytes");
    let frames = decode_envelopes(&bytes);
    assert_eq!(frames.len(), 4, "frames={frames:?}");
    assert_eq!(frames[0].0, 0);
    assert!(frames[0].1.get("start").is_some());
    assert_eq!(frames[1].1["data"]["payload"], "AQID");
    assert_eq!(frames[2].1["end"]["ok"], true);
    assert_eq!(frames[3].0 & 0x02, 0x02);
    handle.abort();
}

#[tokio::test]
async fn connect_json_client_stream_roundtrip() {
    let (port, handle) = spawn_gate_server().await;
    let mut body = encode_envelope(0, json!({"chunk": "AQI="}));
    body.extend(encode_envelope(0, json!({"chunk": "AwQF"})));
    body.extend(encode_envelope(0x02, json!({})));
    let resp = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/conformance.echo.Echo/ClientStream"
        ))
        .header("content-type", "application/connect+json")
        .header("connect-protocol-version", "1")
        .body(body)
        .send()
        .await
        .expect("post");
    assert_eq!(resp.status(), StatusCode::OK);
    let bytes = resp.bytes().await.expect("bytes");
    let frames = decode_envelopes(&bytes);
    assert_eq!(frames[0].1["message"], "bytes=5");
    assert_eq!(frames.last().expect("end stream").0 & 0x02, 0x02);
    handle.abort();
}

#[tokio::test]
async fn malformed_and_oversize_stream_frames_return_domain_errors() {
    let (port, handle) = spawn_gate_server().await;
    let client = reqwest::Client::new();
    let url = format!("http://127.0.0.1:{port}/conformance.echo.Echo/ClientStream");

    let malformed = client
        .post(&url)
        .header("content-type", "application/connect+json")
        .header("connect-protocol-version", "1")
        .body(vec![0, 0, 0, 0, 2, b'{'])
        .send()
        .await
        .expect("malformed frame");
    assert_eq!(malformed.status(), StatusCode::OK);
    let malformed_body = malformed.bytes().await.expect("malformed response body");
    let malformed_frames = decode_envelopes(&malformed_body);
    assert_eq!(
        malformed_frames.last().expect("error trailer").1["error"]["code"],
        "invalid_argument"
    );

    let declared = (64u32 * 1024 * 1024 + 1).to_be_bytes();
    let mut header = vec![0];
    header.extend_from_slice(&declared);
    let oversize = client
        .post(&url)
        .header("content-type", "application/connect+json")
        .header("connect-protocol-version", "1")
        .body(header)
        .send()
        .await
        .expect("oversize frame");
    assert_eq!(oversize.status(), StatusCode::OK);
    let oversize_body = oversize.bytes().await.expect("oversize response body");
    let oversize_frames = decode_envelopes(&oversize_body);
    assert_eq!(
        oversize_frames.last().expect("error trailer").1["error"]["code"],
        "invalid_argument"
    );
    handle.abort();
}

#[tokio::test]
async fn decoded_stream_element_budget_is_resource_exhausted() {
    let limits = connectrpc::Limits::unlimited()
        .with_max_request_body_size(1024)
        .with_max_message_size(1024)
        .with_element_memory_limit(1);
    let (port, handle) = spawn_limited_gate_server(limits).await;
    let mut body = encode_raw_envelope(0, &[0x12, 0]);
    body.extend(encode_raw_envelope(0x02, &[]));
    let response = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/conformance.echo.Echo/ClientStream"
        ))
        .header("content-type", "application/connect+proto")
        .header("connect-protocol-version", "1")
        .body(body)
        .send()
        .await
        .expect("post");
    assert_eq!(response.status(), StatusCode::OK);
    let frames = decode_envelopes(&response.bytes().await.expect("Connect response"));
    assert_eq!(
        frames.last().expect("error trailer").1["error"]["code"],
        "resource_exhausted"
    );
    handle.abort();
}

#[tokio::test]
async fn connect_request_body_limit_is_resource_exhausted() {
    let limits = connectrpc::Limits::unlimited()
        .with_max_request_body_size(64)
        .with_max_message_size(1024)
        .with_element_memory_limit(1024);
    let (port, handle) = spawn_limited_gate_server(limits).await;
    let response = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/conformance.echo.Echo/Unary"
        ))
        .header("content-type", "application/json")
        .header("connect-protocol-version", "1")
        .body(format!(r#"{{"message":"{}"}}"#, "a".repeat(64)))
        .send()
        .await
        .expect("post");
    assert_eq!(response.status(), StatusCode::TOO_MANY_REQUESTS);
    let error: Value = response.json().await.expect("Connect error JSON");
    assert_eq!(error["code"], "resource_exhausted");
    handle.abort();
}

#[tokio::test]
async fn decoded_message_limit_is_resource_exhausted() {
    let limits = connectrpc::Limits::unlimited()
        .with_max_request_body_size(1024)
        .with_max_message_size(64)
        .with_element_memory_limit(1024);
    let (port, handle) = spawn_limited_gate_server(limits).await;
    let response = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/conformance.echo.Echo/Unary"
        ))
        .header("content-type", "application/json")
        .header("connect-protocol-version", "1")
        .body(format!(r#"{{"message":"{}"}}"#, "a".repeat(64)))
        .send()
        .await
        .expect("post");
    assert_eq!(response.status(), StatusCode::TOO_MANY_REQUESTS);
    let error: Value = response.json().await.expect("Connect error JSON");
    assert_eq!(error["code"], "resource_exhausted");
    handle.abort();
}

#[tokio::test]
async fn decompressed_message_limit_is_resource_exhausted() {
    const GZIP_JSON_OVER_64_BYTES: &[u8] = &[
        0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xab, 0x56, 0xca, 0x4d, 0x2d,
        0x2e, 0x4e, 0x4c, 0x4f, 0x55, 0xb2, 0x52, 0x4a, 0x1c, 0x26, 0x40, 0xa9, 0x16, 0x00, 0xf5,
        0xa7, 0x19, 0x90, 0xd6, 0x00, 0x00, 0x00,
    ];
    let limits = connectrpc::Limits::unlimited()
        .with_max_request_body_size(1024)
        .with_max_message_size(64)
        .with_element_memory_limit(1024);
    let (port, handle) = spawn_limited_gate_server(limits).await;
    let response = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/conformance.echo.Echo/Unary"
        ))
        .header("content-type", "application/json")
        .header("content-encoding", "gzip")
        .header("connect-protocol-version", "1")
        .body(GZIP_JSON_OVER_64_BYTES)
        .send()
        .await
        .expect("post");
    assert_eq!(response.status(), StatusCode::TOO_MANY_REQUESTS);
    let error: Value = response.json().await.expect("Connect error JSON");
    assert_eq!(error["code"], "resource_exhausted");
    handle.abort();
}

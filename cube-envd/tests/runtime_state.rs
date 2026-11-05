// SPDX-License-Identifier: Apache-2.0

use std::sync::Arc;

use reqwest::{Response, StatusCode};
use tokio::io::AsyncWriteExt;
use tokio::net::TcpStream;
use tokio::time::{sleep, timeout, Duration};

use cube_envd::runtime::RuntimeStateStore;
use cube_envd::server::ServerPhase;

mod support;

async fn post_init(client: &reqwest::Client, port: u16, body: &str) -> Response {
    client
        .post(format!("http://127.0.0.1:{port}/init"))
        .body(body.to_owned())
        .send()
        .await
        .expect("init response")
}

#[tokio::test]
async fn initial_runtime_state_has_production_defaults_and_immutable_snapshots() {
    let store = RuntimeStateStore::new();
    let first = store.snapshot().await;
    let second = store.snapshot().await;

    assert!(Arc::ptr_eq(&first, &second));
    assert_eq!(first.default_user(), "root");
    assert_eq!(first.default_workdir(), None);
    assert_eq!(
        first.environment().get("E2B_SANDBOX"),
        Some(&"false".to_owned())
    );
    assert_eq!(first.last_successful_init(), None);
    assert_eq!(first.generation(), 0);
}

#[tokio::test]
async fn env_and_logical_defaults_commit_atomically_with_effective_generation() {
    let (port, state, handle) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = reqwest::Client::new();
    let before = state.runtime.snapshot().await;

    let response = post_init(
        &client,
        port,
        r#"{"envVars":{"A":"1","EMPTY":""},"defaultWorkdir":"~/work/../logical"}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    let first = state.runtime.snapshot().await;
    assert!(!Arc::ptr_eq(&before, &first));
    assert_eq!(before.environment().get("A"), None);
    assert_eq!(first.environment().get("A").map(String::as_str), Some("1"));
    assert_eq!(
        first.environment().get("EMPTY").map(String::as_str),
        Some("")
    );
    assert_eq!(first.default_workdir(), Some("~/work/../logical"));
    assert_eq!(first.generation(), 1);

    assert_eq!(
        post_init(
            &client,
            port,
            r#"{"envVars":{"A":"1","EMPTY":""},"defaultWorkdir":"~/work/../logical"}"#,
        )
        .await
        .status(),
        StatusCode::NO_CONTENT
    );
    let repeated = state.runtime.snapshot().await;
    assert!(Arc::ptr_eq(&first, &repeated));
    assert_eq!(repeated.generation(), 1);

    assert_eq!(
        post_init(&client, port, r#"{"envVars":{"A":"2"}}"#)
            .await
            .status(),
        StatusCode::NO_CONTENT
    );
    let merged = state.runtime.snapshot().await;
    assert_eq!(merged.environment().get("A").map(String::as_str), Some("2"));
    assert_eq!(
        merged.environment().get("EMPTY").map(String::as_str),
        Some("")
    );
    assert_eq!(merged.generation(), 2);

    assert_eq!(
        post_init(
            &client,
            port,
            r#"{"defaultUser":"","defaultWorkdir":null,"envVars":null}"#,
        )
        .await
        .status(),
        StatusCode::NO_CONTENT
    );
    assert!(Arc::ptr_eq(&merged, &state.runtime.snapshot().await));
    handle.abort();
}

#[tokio::test]
async fn existing_default_user_and_all_logical_workdir_forms_are_preserved() {
    let (port, state, handle) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = reqwest::Client::new();

    assert_eq!(
        post_init(&client, port, r#"{"defaultUser":"nobody"}"#)
            .await
            .status(),
        StatusCode::NO_CONTENT
    );
    assert_eq!(state.runtime.snapshot().await.default_user(), "nobody");

    for (generation, workdir) in [
        (2, "/absolute/path"),
        (3, "relative/path"),
        (4, "~/tilde/path"),
    ] {
        let body = serde_json::json!({"defaultWorkdir": workdir}).to_string();
        assert_eq!(
            post_init(&client, port, &body).await.status(),
            StatusCode::NO_CONTENT
        );
        let snapshot = state.runtime.snapshot().await;
        assert_eq!(snapshot.default_workdir(), Some(workdir));
        assert_eq!(snapshot.generation(), generation);
    }

    let response = post_init(
        &client,
        port,
        r#"{"defaultUser":"cube-envd-user-that-does-not-exist"}"#,
    )
    .await;
    assert_eq!(response.status(), StatusCode::NO_CONTENT);
    assert_eq!(
        state.runtime.snapshot().await.default_user(),
        "cube-envd-user-that-does-not-exist"
    );
    handle.abort();
}

#[tokio::test]
async fn timestamp_orders_absolute_nanosecond_instants_before_semantic_validation() {
    let (port, state, handle) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = reqwest::Client::new();

    assert_eq!(
        post_init(
            &client,
            port,
            r#"{"timestamp":"2026-01-01T00:00:00.123456789999Z","envVars":{"A":"first"}}"#,
        )
        .await
        .status(),
        StatusCode::NO_CONTENT
    );
    let first = state.runtime.snapshot().await;
    assert_eq!(
        first.last_successful_init().unwrap().to_string(),
        "2026-01-01T00:00:00.123456789Z"
    );
    assert_eq!(first.generation(), 1);

    let stale_with_semantic_error = post_init(
        &client,
        port,
        r#"{"timestamp":"2025-12-31T19:00:00.123456788001-05:00","defaultUser":"missing-user"}"#,
    )
    .await;
    assert_eq!(stale_with_semantic_error.status(), StatusCode::NO_CONTENT);
    assert!(Arc::ptr_eq(&first, &state.runtime.snapshot().await));

    assert_eq!(
        post_init(
            &client,
            port,
            r#"{"timestamp":"2026-01-01T00:00:00.123456790Z"}"#,
        )
        .await
        .status(),
        StatusCode::NO_CONTENT
    );
    let newer = state.runtime.snapshot().await;
    assert_eq!(newer.generation(), 2);
    assert_eq!(
        newer.environment().get("A").map(String::as_str),
        Some("first")
    );
    handle.abort();
}

#[tokio::test]
async fn concurrent_timestamp_requests_linearize_to_the_newest_state() {
    let (port, state, handle) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let client = reqwest::Client::new();
    let older = post_init(
        &client,
        port,
        r#"{"timestamp":"2026-01-01T00:00:00Z","envVars":{"ORDER":"older"}}"#,
    );
    let newer = post_init(
        &client,
        port,
        r#"{"timestamp":"2026-01-02T00:00:00Z","envVars":{"ORDER":"newer"}}"#,
    );
    let (older, newer) = tokio::join!(older, newer);
    assert_eq!(older.status(), StatusCode::NO_CONTENT);
    assert_eq!(newer.status(), StatusCode::NO_CONTENT);
    let snapshot = state.runtime.snapshot().await;
    assert_eq!(
        snapshot.environment().get("ORDER").map(String::as_str),
        Some("newer")
    );
    assert_eq!(
        snapshot.last_successful_init().unwrap().to_string(),
        "2026-01-02T00:00:00Z"
    );
    handle.abort();
}

#[tokio::test]
async fn interrupted_body_before_commit_does_not_mutate_state() {
    let (port, state, handle) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let mut stream = TcpStream::connect(("127.0.0.1", port))
        .await
        .expect("connect");
    stream
        .write_all(
            b"POST /init HTTP/1.1\r\nHost: localhost\r\nContent-Length: 128\r\nConnection: close\r\n\r\n{\"envVars\":{\"CANCELLED\":\"yes\"",
        )
        .await
        .expect("write partial request");
    stream.shutdown().await.expect("shutdown partial request");
    drop(stream);
    sleep(Duration::from_millis(25)).await;

    let snapshot = state.runtime.snapshot().await;
    assert_eq!(snapshot.environment().get("CANCELLED"), None);
    assert_eq!(snapshot.generation(), 0);
    handle.abort();
}

#[tokio::test]
async fn response_loss_after_complete_request_keeps_the_commit() {
    let (port, state, handle) = support::spawn_envd_with_state(ServerPhase::Ready).await;
    let body = r#"{"envVars":{"RESPONSE_LOST":"committed"}}"#;
    let request = format!(
        "POST /init HTTP/1.1\r\nHost: localhost\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        body.len(),
        body
    );
    let mut stream = TcpStream::connect(("127.0.0.1", port))
        .await
        .expect("connect");
    stream
        .write_all(request.as_bytes())
        .await
        .expect("write complete request");

    timeout(Duration::from_secs(1), async {
        loop {
            if state
                .runtime
                .snapshot()
                .await
                .environment()
                .get("RESPONSE_LOST")
                .is_some()
            {
                break;
            }
            sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("commit after response loss");
    drop(stream);
    let snapshot = state.runtime.snapshot().await;
    assert_eq!(
        snapshot
            .environment()
            .get("RESPONSE_LOST")
            .map(String::as_str),
        Some("committed")
    );
    assert_eq!(snapshot.generation(), 1);
    handle.abort();
}

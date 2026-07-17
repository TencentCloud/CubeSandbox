// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use axum::body::{to_bytes, Body};
use axum::http::{Request, StatusCode};
use crash_recovery_worker::{app, State, Terminator, Worker};
use tower::ServiceExt;

struct Noop;

impl Terminator for Noop {
    fn terminate(&self) {}
}

struct Recorder {
    called: AtomicBool,
}

impl Recorder {
    fn new() -> Self {
        Self {
            called: AtomicBool::new(false),
        }
    }
}

impl Terminator for Recorder {
    fn terminate(&self) {
        self.called.store(true, Ordering::SeqCst);
    }
}

#[tokio::test]
async fn health_endpoint_reports_ready() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let worker = Worker::open(dir.path().join("audit.jsonl")).expect("open worker");
    let app = app(worker, Arc::new(Noop));

    let response = app
        .oneshot(
            Request::builder()
                .uri("/health")
                .body(Body::empty())
                .expect("build request"),
        )
        .await
        .expect("call endpoint");

    assert_eq!(response.status(), StatusCode::OK);

    let body = to_bytes(response.into_body(), 1_024)
        .await
        .expect("read response body");
    assert_eq!(body.as_ref(), br#"{"status":"ok"}"#);
}

#[tokio::test]
async fn state_endpoint_returns_the_complete_worker_state() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let worker = Worker::open(dir.path().join("audit.jsonl")).expect("open worker");
    let app = app(worker, Arc::new(Noop));

    let response = app
        .oneshot(
            Request::builder()
                .uri("/state")
                .body(Body::empty())
                .expect("build request"),
        )
        .await
        .expect("call endpoint");

    assert_eq!(response.status(), StatusCode::OK);

    let body = to_bytes(response.into_body(), 16 * 1_024)
        .await
        .expect("read response body");
    let state: State = serde_json::from_slice(&body).expect("parse worker state");

    assert_eq!(state.initial_balances, state.balances);
    assert_eq!(state.balances.values().sum::<i64>(), 1_750);
    assert!(state.pending.is_empty());
    assert!(state.ledger.is_empty());
    assert!(state.seen.is_empty());
}

#[tokio::test]
async fn transfer_endpoint_commits_and_returns_created() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let worker = Worker::open(dir.path().join("audit.jsonl")).expect("open worker");
    let app = app(worker, Arc::new(Noop));
    let request = br#"{"id":"tx-001","from":"alice","to":"bob","amount":100}"#;

    let response = app
        .clone()
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/transfers")
                .header("content-type", "application/json")
                .body(Body::from(request.as_slice()))
                .expect("build request"),
        )
        .await
        .expect("call endpoint");

    assert_eq!(response.status(), StatusCode::CREATED);

    let body = to_bytes(response.into_body(), 1_024)
        .await
        .expect("read response body");
    let result: serde_json::Value = serde_json::from_slice(&body).expect("parse result");
    assert_eq!(result["outcome"], "committed");

    let response = app
        .oneshot(
            Request::builder()
                .uri("/state")
                .body(Body::empty())
                .expect("build request"),
        )
        .await
        .expect("call state endpoint");
    let body = to_bytes(response.into_body(), 16 * 1_024)
        .await
        .expect("read response body");
    let state: State = serde_json::from_slice(&body).expect("parse worker state");

    assert_eq!(state.balances["alice"], 900);
    assert_eq!(state.balances["bob"], 600);
    assert_eq!(state.ledger.len(), 1);
}

#[tokio::test]
async fn fault_header_persists_dirty_state_before_terminating() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let worker = Worker::open(dir.path().join("audit.jsonl")).expect("open worker");
    let terminator = Arc::new(Recorder::new());
    let app = app(worker, terminator.clone());
    let request = br#"{"id":"tx-crash","from":"alice","to":"bob","amount":100}"#;

    let response = app
        .clone()
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/transfers")
                .header("content-type", "application/json")
                .header("x-fault-point", "after_debit")
                .body(Body::from(request.as_slice()))
                .expect("build request"),
        )
        .await
        .expect("call endpoint");

    assert_eq!(response.status(), StatusCode::INTERNAL_SERVER_ERROR);
    assert!(terminator.called.load(Ordering::SeqCst));

    let response = app
        .oneshot(
            Request::builder()
                .uri("/state")
                .body(Body::empty())
                .expect("build request"),
        )
        .await
        .expect("call state endpoint");
    let body = to_bytes(response.into_body(), 16 * 1_024)
        .await
        .expect("read response body");
    let state: State = serde_json::from_slice(&body).expect("parse worker state");

    assert_eq!(state.balances.values().sum::<i64>(), 1_650);
    assert!(state.pending.contains_key("tx-crash"));
    assert!(state.ledger.is_empty());
}

#[tokio::test]
async fn invalid_transfer_returns_a_structured_error() {
    let dir = tempfile::tempdir().expect("create temporary directory");
    let worker = Worker::open(dir.path().join("audit.jsonl")).expect("open worker");
    let app = app(worker, Arc::new(Noop));
    let request = br#"{"id":"tx-invalid","from":"alice","to":"bob","amount":0}"#;

    let response = app
        .oneshot(
            Request::builder()
                .method("POST")
                .uri("/transfers")
                .header("content-type", "application/json")
                .body(Body::from(request.as_slice()))
                .expect("build request"),
        )
        .await
        .expect("call endpoint");

    assert_eq!(response.status(), StatusCode::BAD_REQUEST);

    let body = to_bytes(response.into_body(), 4 * 1_024)
        .await
        .expect("read response body");
    let result: serde_json::Value = serde_json::from_slice(&body).expect("parse error");

    assert_eq!(result["error"]["code"], "invalid_amount");
    assert_eq!(result["error"]["message"], "amount must be positive");
}

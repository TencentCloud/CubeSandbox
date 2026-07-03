//! Minimal axum HTTP server pre-compiled in the CubeSandbox Rust template.
//!
//! This binary is built once during `docker build` to warm the cargo registry
//! and compilation cache. Inside a running sandbox, users can rebuild it after
//! making changes, or use `cargo new` to create their own projects.

use axum::{routing::get, Json, Router};
use serde::Serialize;
use std::net::SocketAddr;

#[derive(Serialize)]
struct HealthResponse {
    status: &'static str,
    runtime: &'static str,
    message: &'static str,
}

async fn health() -> Json<HealthResponse> {
    Json(HealthResponse {
        status: "ok",
        runtime: "rust",
        message: "Hello from Rust inside CubeSandbox!",
    })
}

#[tokio::main]
async fn main() {
    let app = Router::new().route("/", get(health));

    let addr = SocketAddr::from(([0, 0, 0, 0], 8080));
    println!("Rust demo server listening on {addr}");

    let listener = tokio::net::TcpListener::bind(addr).await.unwrap();
    axum::serve(listener, app).await.unwrap();
}

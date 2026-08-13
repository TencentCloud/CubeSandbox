// Webhook receiver — a minimal axum server that receives and validates
// webhook POST requests from CubeAPI.
//
// Usage:
//   WEBHOOK_SECRET=your-secret cargo run
//
// Default: 127.0.0.1:9090. Override: PORT=8080 LISTEN=0.0.0.0

use axum::{routing::{get, post}, Router};
use hmac::{Hmac, Mac};
use sha2::Sha256;
use std::env;

type HmacSha256 = Hmac<Sha256>;

#[tokio::main]
async fn main() {
    let secret = env::var("WEBHOOK_SECRET").unwrap_or_default();
    let secret = std::sync::Arc::new(secret);
    let listen = env::var("LISTEN").unwrap_or_else(|_| "127.0.0.1".to_string());
    let port = env::var("PORT").unwrap_or_else(|_| "9090".to_string());
    let addr = format!("{listen}:{port}");

    let has_secret = !secret.is_empty();

    let app = Router::new()
        .route("/webhook", post(handle_webhook))
        .route("/health", get(|| async { "ok" }))
        .with_state(secret);

    let listener = tokio::net::TcpListener::bind(&addr).await.unwrap();
    println!("webhook-receiver listening on http://{addr}");
    println!("  POST /webhook — receive webhook events");
    println!("  GET  /health  — health check");
    if has_secret {
        println!("  HMAC verification: enabled");
    }
    axum::serve(listener, app).await.unwrap();
}

async fn handle_webhook(
    axum::extract::State(secret): axum::extract::State<std::sync::Arc<String>>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> (axum::http::StatusCode, String) {
    // Verify HMAC signature FIRST (if configured)
    if !secret.is_empty() {
        let sig_header = match headers.get("X-Cube-Signature").and_then(|v| v.to_str().ok()) {
            Some(v) => v,
            None => {
                println!("=== Webhook REJECTED (missing X-Cube-Signature) ===");
                return (
                    axum::http::StatusCode::UNAUTHORIZED,
                    "X-Cube-Signature header missing".to_string(),
                );
            }
        };

        let sig_hex = match sig_header.strip_prefix("sha256=") {
            Some(hex) => hex,
            None => {
                println!("=== Webhook REJECTED (invalid signature format) ===");
                return (
                    axum::http::StatusCode::UNAUTHORIZED,
                    "invalid signature format".to_string(),
                );
            }
        };

        let sig_bytes = match hex::decode(sig_hex) {
            Ok(b) => b,
            Err(_) => {
                println!("=== Webhook REJECTED (invalid signature encoding) ===");
                return (
                    axum::http::StatusCode::UNAUTHORIZED,
                    "invalid signature encoding".to_string(),
                );
            }
        };

        let mut mac = HmacSha256::new_from_slice(secret.as_bytes())
            .expect("HMAC can take key of any size");
        mac.update(&body);

        if mac.verify_slice(&sig_bytes).is_err() {
            println!("=== Webhook REJECTED (signature mismatch) ===");
            return (
                axum::http::StatusCode::UNAUTHORIZED,
                "signature mismatch".to_string(),
            );
        }
    }

    // Print only after validation passes
    println!("=== Webhook Received ===");
    for (name, value) in headers.iter() {
        if name.as_str().starts_with("x-cube-") || name.as_str() == "content-type" {
            println!("  {}: {}", name, value.to_str().unwrap_or("<non-utf8>"));
        }
    }

    let body_str = String::from_utf8_lossy(&body);
    if let Ok(parsed) = serde_json::from_str::<serde_json::Value>(&body_str) {
        println!("{}", serde_json::to_string_pretty(&parsed).unwrap());
    } else {
        println!("{}", body_str);
    }
    println!("========================\n");

    forward_to_wecom(&body_str).await;

    (axum::http::StatusCode::OK, "ok".to_string())
}

/// Forward to WeCom bot if WECOM_WEBHOOK_URL is set.
async fn forward_to_wecom(body_str: &str) {
    let wecom_url = match std::env::var("WECOM_WEBHOOK_URL") {
        Ok(url) if !url.is_empty() => url,
        _ => return,
    };
    let parsed = match serde_json::from_str::<serde_json::Value>(body_str) {
        Ok(v) => v,
        Err(_) => return,
    };
    let content = format!(
        "【CubeSandbox】{}\nSandbox: {}\nTemplate: {}\nTime: {}",
        parsed["event"].as_str().unwrap_or("unknown"),
        parsed["sandbox_id"].as_str().unwrap_or("?"),
        parsed["template_id"].as_str().unwrap_or("N/A"),
        parsed["timestamp"].as_str().unwrap_or("?")
    );
    let _ = reqwest::Client::new()
        .post(&wecom_url)
        .json(&serde_json::json!({"msgtype": "text", "text": {"content": content}}))
        .send()
        .await;
}


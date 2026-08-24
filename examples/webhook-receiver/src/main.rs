// Webhook receiver — a minimal axum server that receives and validates
// webhook POST requests from CubeOps (proposal/webhook-delivery-spec.md §6).
//
// Validation steps (in order):
//   1. HMAC-SHA256 signature over the RAW body bytes, hex in
//      X-Cube-Signature-256, compared in constant time (401 on mismatch).
//   2. X-Cube-Timestamp within the tolerance window (default ±5 minutes).
//   3. Idempotency: repeated X-Cube-Delivery values are acknowledged without
//      re-processing (in-memory dedup set).
//
// Usage:
//   WEBHOOK_SECRET=your-secret cargo run
//
// Default: 127.0.0.1:9090. Override: PORT=8080 LISTEN=0.0.0.0

use axum::{extract::State, http::StatusCode, routing::{get, post}, Router};
use hmac::{Hmac, Mac};
use sha2::Sha256;
use std::{collections::HashSet, env, sync::{Arc, Mutex}};

type HmacSha256 = Hmac<Sha256>;

const DEFAULT_TOLERANCE_SECS: i64 = 300;

/// verify_signature computes hex(HMAC-SHA256(secret, body)) and compares it
/// to the provided hex signature in constant time.
fn verify_signature(secret: &str, body: &[u8], sig_hex: &str) -> bool {
    let sig_bytes = match hex::decode(sig_hex.trim()) {
        Ok(b) => b,
        Err(_) => return false,
    };
    let mut mac = HmacSha256::new_from_slice(secret.as_bytes())
        .expect("HMAC accepts keys of any size");
    mac.update(body);
    mac.verify_slice(&sig_bytes).is_ok()
}

/// in_tolerance checks |now_ms - header_ms| <= tolerance_secs.
fn in_tolerance(now_ms: i64, header_ms: i64, tolerance_secs: i64) -> bool {
    (now_ms - header_ms).abs() <= tolerance_secs * 1000
}

#[tokio::main]
async fn main() {
    let secret = env::var("WEBHOOK_SECRET").unwrap_or_default();
    let listen = env::var("LISTEN").unwrap_or_else(|_| "127.0.0.1".to_string());
    let port = env::var("PORT").unwrap_or_else(|_| "9090".to_string());
    let addr = format!("{listen}:{port}");
    let tolerance = env::var("TIMESTAMP_TOLERANCE_SECS")
        .ok()
        .and_then(|v| v.parse::<i64>().ok())
        .unwrap_or(DEFAULT_TOLERANCE_SECS);

    let app = Router::new()
        .route("/webhook", post(handle_webhook))
        .route("/health", get(|| async { "ok" }))
        .with_state(AppState {
            secret: Arc::new(secret),
            tolerance,
            seen: Arc::new(Mutex::new(HashSet::new())),
        });

    let listener = tokio::net::TcpListener::bind(&addr).await.unwrap();
    println!("webhook-receiver listening on http://{addr}");
    println!("  POST /webhook — receive webhook events");
    println!("  GET  /health  — health check");
    println!("  HMAC verification: {}", if !env::var("WEBHOOK_SECRET").unwrap_or_default().is_empty() { "enabled" } else { "disabled" });
    axum::serve(listener, app).await.unwrap();
}

#[derive(Clone)]
struct AppState {
    secret: Arc<String>,
    tolerance: i64,
    seen: Arc<Mutex<HashSet<String>>>,
}

async fn handle_webhook(
    State(state): State<AppState>,
    headers: axum::http::HeaderMap,
    body: axum::body::Bytes,
) -> (StatusCode, String) {
    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0);

    // Step 1: HMAC signature (raw body bytes).
    if !state.secret.is_empty() {
        let sig = match headers.get("X-Cube-Signature-256").and_then(|v| v.to_str().ok()) {
            Some(v) => v,
            None => {
                println!("=== Webhook REJECTED (missing X-Cube-Signature-256) ===");
                return (StatusCode::UNAUTHORIZED, "X-Cube-Signature-256 missing".to_string());
            }
        };
        if !verify_signature(&state.secret, &body, sig) {
            println!("=== Webhook REJECTED (signature mismatch) ===");
            return (StatusCode::UNAUTHORIZED, "signature mismatch".to_string());
        }
    }

    // Step 2: timestamp tolerance (anti-replay).
    if let Some(ts) = headers.get("X-Cube-Timestamp").and_then(|v| v.to_str().ok()) {
        if let Ok(ms) = ts.trim().parse::<i64>() {
            if !in_tolerance(now_ms, ms, state.tolerance) {
                println!("=== Webhook REJECTED (timestamp outside tolerance) ===");
                return (StatusCode::UNAUTHORIZED, "timestamp outside tolerance".to_string());
            }
        } else {
            println!("=== Webhook REJECTED (invalid X-Cube-Timestamp) ===");
            return (StatusCode::UNAUTHORIZED, "invalid timestamp".to_string());
        }
    }

    // Step 3: idempotency by X-Cube-Delivery.
    let delivery = headers
        .get("X-Cube-Delivery")
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_string();
    if !delivery.is_empty() {
        let mut seen = state.seen.lock().unwrap();
        if !seen.insert(delivery.clone()) {
            println!("=== Webhook DUPLICATE (already processed {delivery}) — acked ===");
            return (StatusCode::OK, "ok".to_string());
        }
    }

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
        println!("{body_str}");
    }
    println!("========================\n");

    forward_to_wecom(&body_str).await;

    (StatusCode::OK, "ok".to_string())
}

/// Forward to WeCom bot if WECOM_WEBHOOK_URL is set.
async fn forward_to_wecom(body_str: &str) {
    let wecom_url = match env::var("WECOM_WEBHOOK_URL") {
        Ok(url) if !url.is_empty() => url,
        _ => return,
    };
    let parsed = match serde_json::from_str::<serde_json::Value>(body_str) {
        Ok(v) => v,
        Err(_) => return,
    };
    let timestamp = parsed["timestamp"].as_i64().unwrap_or(0);
    let time_str = if timestamp > 0 {
        format!("{}", timestamp)
    } else {
        "?".to_string()
    };
    let content = format!(
        "【CubeSandbox】{}\nSandbox: {}\nTemplate: {}\nTime: {}",
        parsed["event"].as_str().unwrap_or("unknown"),
        parsed["sandbox_id"].as_str().unwrap_or("?"),
        parsed["template_id"].as_str().unwrap_or("N/A"),
        time_str,
    );
    let _ = reqwest::Client::new()
        .post(&wecom_url)
        .json(&serde_json::json!({"msgtype": "text", "text": {"content": content}}))
        .send()
        .await;
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hmac_hex(secret: &str, body: &[u8]) -> String {
        let mut mac = HmacSha256::new_from_slice(secret.as_bytes()).unwrap();
        mac.update(body);
        hex::encode(mac.finalize().into_bytes())
    }

    #[test]
    fn signature_valid_and_invalid() {
        let body = br#"{"event":"sandbox.created"}"#;
        let good = hmac_hex("s3cr3t", body);
        assert!(verify_signature("s3cr3t", body, &good));
        assert!(!verify_signature("s3cr3t", body, "deadbeef"));
        assert!(!verify_signature("s3cr3t", body, "not-hex"));
        assert!(!verify_signature("other", body, &good));
    }

    #[test]
    fn timestamp_tolerance() {
        let now = 1_700_000_000_000_i64;
        assert!(in_tolerance(now, now, 300));
        assert!(in_tolerance(now, now - 299_000, 300));
        assert!(!in_tolerance(now, now - 301_000, 300));
        assert!(!in_tolerance(now, now + 301_000, 300));
    }
}

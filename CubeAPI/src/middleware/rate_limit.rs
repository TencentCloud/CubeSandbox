// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::error::AppError;
use crate::state::AppState;
use axum::{
    extract::{Request, State},
    http::Method,
    middleware::Next,
    response::Response,
};

/// Per-API-key token bucket rate limiter middleware.
/// Reads the X-API-Key header and checks the governor limiter for the
/// request's class: read requests (GET/HEAD) use `RATE_LIMIT_READ_PER_SEC`,
/// write requests (POST/PUT/PATCH/DELETE) use `RATE_LIMIT_WRITE_PER_SEC`,
/// and any class without a dedicated quota — as well as methods outside both
/// lists — falls back to the shared `RATE_LIMIT_PER_SEC` limiter.
/// Returns 429 if the key has exceeded its class quota.
pub async fn rate_limit(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    // Extract key; fall back to IP or "anonymous"
    let key = request
        .headers()
        .get("X-API-Key")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("anonymous")
        .to_string();

    match limiter_for_method(&state, request.method()).check_key(&key) {
        Ok(_) => Ok(next.run(request).await),
        Err(_) => Err(AppError::TooManyRequests(
            "Rate limit exceeded. Slow down.".to_string(),
        )),
    }
}

/// Pick the token bucket for a request method: the dedicated read/write
/// limiter when configured, otherwise the shared limiter.
fn limiter_for_method<'a>(
    state: &'a AppState,
    method: &Method,
) -> &'a governor::DefaultKeyedRateLimiter<String> {
    match *method {
        Method::GET | Method::HEAD => state.read_rate_limiter.as_deref(),
        Method::POST | Method::PUT | Method::PATCH | Method::DELETE => {
            state.write_rate_limiter.as_deref()
        }
        _ => None,
    }
    .unwrap_or(&state.rate_limiter)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        config::ServerConfig,
        logging::{arc, noop::NoopLogger},
        state::AppState,
    };
    use axum::{
        http::{HeaderName, HeaderValue, Method, StatusCode},
        middleware,
        routing::get,
        Router,
    };
    use axum_test::TestServer;

    const KEY: &str = "test-key";
    const API_KEY_HEADER: &str = "x-api-key";

    async fn test_server(config: ServerConfig) -> TestServer {
        let state = AppState::new(config, arc(NoopLogger)).await;
        let app = Router::new()
            .route(
                "/ping",
                get(ping).post(ping).put(ping).patch(ping).delete(ping),
            )
            .layer(middleware::from_fn_with_state(state.clone(), rate_limit))
            .with_state(state);
        TestServer::new(app).expect("router should build")
    }

    async fn ping() -> &'static str {
        "ok"
    }

    fn test_config() -> ServerConfig {
        ServerConfig::default()
    }

    async fn request(server: &TestServer, method: Method, path: &str) -> axum_test::TestResponse {
        server
            .method(method, path)
            .add_header(
                HeaderName::from_static(API_KEY_HEADER),
                HeaderValue::from_static(KEY),
            )
            .await
    }

    /// Backwards compatibility: with neither new setting configured, every
    /// method consumes the shared `RATE_LIMIT_PER_SEC` bucket exactly as
    /// before.
    #[tokio::test]
    async fn shared_limit_applies_to_all_methods_when_split_not_configured() {
        let mut config = test_config();
        config.rate_limit_per_sec = 1;
        let server = test_server(config).await;

        let first = request(&server, Method::GET, "/ping").await;
        first.assert_status_ok();

        let second = request(&server, Method::POST, "/ping").await;
        second.assert_status(StatusCode::TOO_MANY_REQUESTS);
    }

    /// Core scenario from the issue: status polling (reads) exhausting the
    /// shared quota must not throttle lifecycle operations (writes) once the
    /// classes have dedicated buckets.
    #[tokio::test]
    async fn read_exhaustion_does_not_throttle_writes() {
        let mut config = test_config();
        config.rate_limit_per_sec = 1;
        config.rate_limit_read_per_sec = Some(1);
        config.rate_limit_write_per_sec = Some(100);
        let server = test_server(config).await;

        let first = request(&server, Method::GET, "/ping").await;
        first.assert_status_ok();

        // The read bucket is exhausted…
        let second = request(&server, Method::GET, "/ping").await;
        second.assert_status(StatusCode::TOO_MANY_REQUESTS);

        // …yet writes are unaffected: they consume their own quota, not the
        // read bucket nor the shared bucket.
        let write = request(&server, Method::POST, "/ping").await;
        write.assert_status_ok();
    }

    /// Fallback documented for a single setting: when only the read limit is
    /// configured, reads use their own bucket while writes keep using the
    /// shared `RATE_LIMIT_PER_SEC` bucket.
    #[tokio::test]
    async fn write_falls_back_to_shared_limit_when_only_read_configured() {
        let mut config = test_config();
        config.rate_limit_per_sec = 1;
        config.rate_limit_read_per_sec = Some(100);
        let server = test_server(config).await;

        // Reads ride the dedicated bucket — repeated polling passes.
        let read = request(&server, Method::GET, "/ping").await;
        read.assert_status_ok();

        // Writes fall back to the shared bucket (quota 1): the first passes,
        // the second is throttled.
        let first_write = request(&server, Method::POST, "/ping").await;
        first_write.assert_status_ok();
        let second_write = request(&server, Method::POST, "/ping").await;
        second_write.assert_status(StatusCode::TOO_MANY_REQUESTS);
    }

    /// HEAD belongs to the read class and PUT to the write class: HEAD must
    /// consume the read bucket and PUT the write bucket.
    #[tokio::test]
    async fn head_consumes_read_bucket_and_put_consumes_write_bucket() {
        let mut config = test_config();
        config.rate_limit_read_per_sec = Some(1);
        config.rate_limit_write_per_sec = Some(1);
        let server = test_server(config).await;

        let get = request(&server, Method::GET, "/ping").await;
        get.assert_status_ok();
        let head = request(&server, Method::HEAD, "/ping").await;
        head.assert_status(StatusCode::TOO_MANY_REQUESTS);

        let put = request(&server, Method::PUT, "/ping").await;
        put.assert_status_ok();
        let patch = request(&server, Method::PATCH, "/ping").await;
        patch.assert_status(StatusCode::TOO_MANY_REQUESTS);
    }
}

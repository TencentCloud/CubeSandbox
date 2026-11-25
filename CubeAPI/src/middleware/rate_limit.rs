// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::error::AppError;
use crate::middleware::auth::{extract_credential, AuthCredential};
use crate::state::AppState;
use axum::{
    extract::{Request, State},
    http::Method,
    middleware::Next,
    response::Response,
};

/// Per-credential token bucket rate limiter middleware.
/// Uses the same Bearer-first credential precedence as authentication and
/// checks the governor limiter for the request's class: read requests
/// (GET/HEAD) use `RATE_LIMIT_READ_PER_SEC`,
/// write requests (POST/PUT/PATCH/DELETE) use `RATE_LIMIT_WRITE_PER_SEC`,
/// and any class without a dedicated quota — as well as methods outside both
/// lists — falls back to the shared `RATE_LIMIT_PER_SEC` limiter.
/// Returns 429 if the key has exceeded its class quota.
pub async fn rate_limit(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    // Match unified_auth's Bearer-first precedence. Prefix the credential kind
    // so an API key cannot collide with a Bearer token having the same bytes.
    let key = if state
        .config
        .auth_callback_url
        .as_deref()
        .unwrap_or_default()
        .is_empty()
        && !state
            .config
            .cube_api_key
            .as_deref()
            .unwrap_or_default()
            .is_empty()
    {
        // Simple-key auth accepts the configured secret through either header,
        // so both forms are the same authenticated principal and must share a
        // bucket. Authentication runs before this middleware in production.
        "simple-key".to_string()
    } else {
        match extract_credential(&request) {
            Some(AuthCredential::Bearer(token)) => format!("bearer:{token}"),
            Some(AuthCredential::ApiKey(key)) => format!("api-key:{key}"),
            None => "anonymous".to_string(),
        }
    };

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
        ServerConfig {
            // Keep credential-bucketing tests independent of CUBE_API_KEY in
            // the developer's environment.
            cube_api_key: None,
            auth_callback_url: None,
            ..ServerConfig::default()
        }
    }

    async fn request_with_key(
        server: &TestServer,
        method: Method,
        path: &str,
        key: &'static str,
    ) -> axum_test::TestResponse {
        server
            .method(method, path)
            .add_header(
                HeaderName::from_static(API_KEY_HEADER),
                HeaderValue::from_static(key),
            )
            .await
    }

    async fn request_with_bearer(
        server: &TestServer,
        method: Method,
        token: &str,
        api_key: Option<&str>,
    ) -> axum_test::TestResponse {
        let mut request = server.method(method, "/ping").add_header(
            HeaderName::from_static("authorization"),
            HeaderValue::from_str(&format!("Bearer {token}")).expect("valid test Bearer token"),
        );
        if let Some(api_key) = api_key {
            request = request.add_header(
                HeaderName::from_static(API_KEY_HEADER),
                HeaderValue::from_str(api_key).expect("valid test API key"),
            );
        }
        request.await
    }

    async fn request(server: &TestServer, method: Method, path: &str) -> axum_test::TestResponse {
        request_with_key(server, method, path, KEY).await
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

    /// The split is symmetric: exhausting lifecycle writes must not throttle
    /// status polling. Using DELETE and POST also verifies that both methods
    /// consume the write bucket.
    #[tokio::test]
    async fn write_exhaustion_does_not_throttle_reads() {
        let mut config = test_config();
        config.rate_limit_read_per_sec = Some(100);
        config.rate_limit_write_per_sec = Some(1);
        let server = test_server(config).await;

        let first = request(&server, Method::DELETE, "/ping").await;
        first.assert_status_ok();
        let second = request(&server, Method::POST, "/ping").await;
        second.assert_status(StatusCode::TOO_MANY_REQUESTS);

        let read = request(&server, Method::GET, "/ping").await;
        read.assert_status_ok();
    }

    /// Quotas are keyed independently rather than acting as one global bucket.
    #[tokio::test]
    async fn rate_limit_is_independent_per_api_key() {
        let mut config = test_config();
        config.rate_limit_read_per_sec = Some(1);
        let server = test_server(config).await;

        request_with_key(&server, Method::GET, "/ping", "key-a")
            .await
            .assert_status_ok();
        request_with_key(&server, Method::GET, "/ping", "key-a")
            .await
            .assert_status(StatusCode::TOO_MANY_REQUESTS);
        request_with_key(&server, Method::GET, "/ping", "key-b")
            .await
            .assert_status_ok();
    }

    /// Bearer users get independent buckets, and an unrelated API-key header
    /// cannot change the identity selected by authentication.
    #[tokio::test]
    async fn bearer_identity_matches_auth_precedence() {
        let mut config = test_config();
        config.rate_limit_read_per_sec = Some(1);
        let server = test_server(config).await;

        request_with_bearer(&server, Method::GET, "token-a", None)
            .await
            .assert_status_ok();
        request_with_bearer(&server, Method::GET, "token-a", Some("rotated-key"))
            .await
            .assert_status(StatusCode::TOO_MANY_REQUESTS);
        request_with_bearer(&server, Method::GET, "token-b", None)
            .await
            .assert_status_ok();
    }

    /// Simple-key authentication treats Bearer and API-key forms as aliases
    /// for one configured principal, so switching headers cannot evade quota.
    #[tokio::test]
    async fn simple_key_header_forms_share_a_bucket() {
        let mut config = test_config();
        config.cube_api_key = Some("same-value".to_string());
        config.rate_limit_read_per_sec = Some(1);
        let server = test_server(config).await;

        request_with_bearer(&server, Method::GET, "same-value", None)
            .await
            .assert_status_ok();
        request_with_key(&server, Method::GET, "/ping", "same-value")
            .await
            .assert_status(StatusCode::TOO_MANY_REQUESTS);
    }

    /// Callback authentication can assign different identities to a Bearer
    /// token and API key with identical bytes, so their namespaces stay split.
    #[tokio::test]
    async fn bearer_and_api_key_namespaces_do_not_collide() {
        let mut config = test_config();
        config.rate_limit_read_per_sec = Some(1);
        let server = test_server(config).await;

        request_with_bearer(&server, Method::GET, "same-value", None)
            .await
            .assert_status_ok();
        request_with_key(&server, Method::GET, "/ping", "same-value")
            .await
            .assert_status_ok();
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

    /// Symmetric fallback: when only the write limit is configured, writes
    /// use their own bucket while reads keep using the shared
    /// `RATE_LIMIT_PER_SEC` bucket.
    #[tokio::test]
    async fn read_falls_back_to_shared_limit_when_only_write_configured() {
        let mut config = test_config();
        config.rate_limit_per_sec = 1;
        config.rate_limit_write_per_sec = Some(100);
        let server = test_server(config).await;

        // Writes ride the dedicated bucket — repeated lifecycle calls pass.
        let first_write = request(&server, Method::POST, "/ping").await;
        first_write.assert_status_ok();
        let second_write = request(&server, Method::POST, "/ping").await;
        second_write.assert_status_ok();

        // Reads fall back to the shared bucket (quota 1): the first passes,
        // the second is throttled.
        let first_read = request(&server, Method::GET, "/ping").await;
        first_read.assert_status_ok();
        let second_read = request(&server, Method::GET, "/ping").await;
        second_read.assert_status(StatusCode::TOO_MANY_REQUESTS);
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

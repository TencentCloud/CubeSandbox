// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use crate::error::AppError;
use crate::state::AppState;
use axum::{
    extract::{Request, State},
    middleware::Next,
    response::Response,
};

/// Per-identity token bucket rate limiter middleware.
/// Reads the `RateLimitIdentity` published by `unified_auth` after it validated
/// the credential, and checks the shared governor limiter.
/// Returns 429 if that identity has exceeded its quota.
pub async fn rate_limit(
    State(state): State<AppState>,
    request: Request,
    next: Next,
) -> Result<Response, AppError> {
    let key = request
        .extensions()
        .get::<crate::middleware::auth::RateLimitIdentity>()
        .map(|id| id.0.clone())
        .unwrap_or_else(|| "unauthenticated".to_string());

    match state.rate_limiter.check_key(&key) {
        Ok(_) => Ok(next.run(request).await),
        Err(_) => Err(AppError::TooManyRequests(
            "Rate limit exceeded. Slow down.".to_string(),
        )),
    }
}

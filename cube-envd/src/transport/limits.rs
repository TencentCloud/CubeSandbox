// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use connectrpc::interceptor::{StreamRequest, StreamResponse, UnaryRequest, UnaryResponse};
use connectrpc::{ConnectError, ErrorCode, Interceptor, Next, NextStream, PayloadStream};

/// Default REST `/init` body limit (256 KiB).
pub const INIT_BODY_LIMIT: usize = 256 * 1024;

/// Maximum JSON container nesting for `/init`.
pub const INIT_JSON_DEPTH_LIMIT: usize = 64;

// The upstream handlers do not configure message or request-size limits.
pub fn connect_limits() -> connectrpc::Limits {
    connectrpc::Limits::unlimited()
}

fn classify_budget_exhaustion(mut error: ConnectError) -> ConnectError {
    if error.code == ErrorCode::InvalidArgument
        && error
            .message
            .as_deref()
            .is_some_and(|message| message.contains("element memory limit exceeded"))
    {
        error.code = ErrorCode::ResourceExhausted;
    }
    error
}

pub struct BudgetErrorInterceptor;

#[connectrpc::async_trait]
impl Interceptor for BudgetErrorInterceptor {
    async fn intercept_unary(
        &self,
        request: UnaryRequest,
        next: Next<'_>,
    ) -> Result<UnaryResponse, ConnectError> {
        next.run(request).await.map_err(classify_budget_exhaustion)
    }

    async fn intercept_streaming(
        &self,
        request: StreamRequest,
        inbound: PayloadStream,
        next: NextStream<'_>,
    ) -> Result<StreamResponse, ConnectError> {
        next.run(request, inbound)
            .await
            .map_err(classify_budget_exhaustion)
    }
}

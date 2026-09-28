// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use axum::{
    extract::State,
    http::{header::CONTENT_TYPE, HeaderValue},
    response::IntoResponse,
};

use crate::state::AppState;

pub async fn metrics(State(state): State<AppState>) -> impl IntoResponse {
    let body = state
        .business_metrics
        .render_prometheus()
        .unwrap_or_else(|err| format!("# failed to render metrics: {err}\n"));
    (
        [(
            CONTENT_TYPE,
            HeaderValue::from_static("text/plain; version=0.0.4; charset=utf-8"),
        )],
        body,
    )
}

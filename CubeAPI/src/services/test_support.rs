// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Test-only helpers shared by the service test modules.

use axum::Router;

use crate::{cubemaster::CubeMasterClient, services::snapshots::SnapshotService};

/// Serves `app` on an ephemeral loopback port and returns its base URL.
pub(crate) async fn serve(app: Router) -> String {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("listener should bind");
    let addr = listener.local_addr().expect("listener addr");
    tokio::spawn(async move {
        axum::serve(listener, app).await.expect("server should run");
    });
    format!("http://{addr}")
}

/// A `SnapshotService` pointed at `base`, i.e. at a fake control plane.
pub(crate) fn snapshot_service(base: String) -> SnapshotService {
    SnapshotService::new(
        CubeMasterClient::new(base, reqwest::Client::new()),
        "cubebox".to_string(),
    )
}

// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

//! Connect feasibility gate helpers (test-only echo service).

pub mod echo;

use std::sync::Arc;

use axum::Router;
use connectrpc::Router as ConnectRouter;

use self::echo::EchoGateService;

/// Build a router exposing only the internal echo Connect service plus `/health`.
pub fn build_gate_router() -> Router {
    let echo = Arc::new(EchoGateService);
    let connect = ConnectRouter::new().add_service(echo);
    let lifecycle = Arc::new(crate::server::LifecycleState::new());
    lifecycle.try_transition(crate::server::ServerPhase::Listening);
    lifecycle.try_transition(crate::server::ServerPhase::Ready);
    crate::transport::build_router_with_connect(crate::transport::new_app_state(lifecycle), connect)
}

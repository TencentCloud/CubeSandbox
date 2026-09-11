// SPDX-License-Identifier: Apache-2.0

use std::sync::Arc;

use cube_envd::server::{LifecycleState, ServerPhase};
use cube_envd::transport::rest::AppState;

#[allow(dead_code)]
pub async fn spawn_envd(
    phase: ServerPhase,
) -> (u16, Arc<LifecycleState>, tokio::task::JoinHandle<()>) {
    let (port, state, handle) = spawn_envd_with_state(phase).await;
    (port, state.lifecycle.clone(), handle)
}

pub async fn spawn_envd_with_state(
    phase: ServerPhase,
) -> (u16, Arc<AppState>, tokio::task::JoinHandle<()>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let port = listener.local_addr().expect("addr").port();
    let lifecycle = Arc::new(LifecycleState::new());
    match phase {
        ServerPhase::Booting => {}
        ServerPhase::Listening => {
            assert!(lifecycle.try_transition(ServerPhase::Listening));
        }
        ServerPhase::Ready => {
            assert!(lifecycle.try_transition(ServerPhase::Listening));
            assert!(lifecycle.try_transition(ServerPhase::Ready));
        }
        ServerPhase::Draining => {
            assert!(lifecycle.try_transition(ServerPhase::Listening));
            assert!(lifecycle.try_transition(ServerPhase::Draining));
        }
        ServerPhase::Stopped => {
            assert!(lifecycle.try_transition(ServerPhase::Listening));
            assert!(lifecycle.try_transition(ServerPhase::Draining));
            assert!(lifecycle.try_transition(ServerPhase::Stopped));
        }
        ServerPhase::Failed => {
            lifecycle.fail("test critical task");
        }
    }
    let state = cube_envd::transport::new_app_state(lifecycle);
    let router = cube_envd::transport::build_router(state.clone());
    let handle = tokio::spawn(async move {
        axum::serve(listener, router).await.expect("serve");
    });
    (port, state, handle)
}

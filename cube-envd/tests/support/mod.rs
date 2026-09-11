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

/// Run the Cargo-built daemon for Process wire tests, including startup cgroups.
#[allow(dead_code)]
pub async fn spawn_daemon() -> (u16, tokio::task::JoinHandle<()>) {
    use tokio::io::{AsyncBufReadExt, BufReader};
    let mut child = tokio::process::Command::new(env!("CARGO_BIN_EXE_cube-envd"))
        .args(["-port", "0", "-isnotfc", "--log-format", "json"])
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::inherit())
        .kill_on_drop(true)
        .spawn()
        .expect("spawn Cargo-built daemon");
    let mut lines = BufReader::new(child.stdout.take().unwrap()).lines();
    let port = tokio::time::timeout(std::time::Duration::from_secs(15), async {
        while let Some(line) = lines.next_line().await.unwrap() {
            let log: serde_json::Value = serde_json::from_str(&line).unwrap();
            if log["fields"]["message"] == "envd listening" {
                let address: std::net::SocketAddr =
                    log["fields"]["addr"].as_str().unwrap().parse().unwrap();
                return address.port();
            }
        }
        panic!("daemon exited before listening");
    })
    .await
    .expect("daemon startup");
    let owner = tokio::spawn(async move {
        // Owning Child in this task makes abort close the test daemon as well.
        let draining = async { while lines.next_line().await.unwrap().is_some() {} };
        let (status, ()) = tokio::join!(child.wait(), draining);
        assert!(status.unwrap().success());
    });
    (port, owner)
}

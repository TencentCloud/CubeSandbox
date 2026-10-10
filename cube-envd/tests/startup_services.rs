// SPDX-License-Identifier: Apache-2.0

#[test]
fn daemon_guest_startup_services() {
    let status = std::process::Command::new("python3")
        .arg(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/startup_services.py"
        ))
        .arg("StartupServices")
        .env("ENVD_TEST_BINARY", env!("CARGO_BIN_EXE_cube-envd"))
        .status()
        .expect("run public daemon startup services");
    assert!(status.success());
}

#[tokio::test]
async fn unexpected_guest_completion_fails_the_server_and_stops_other_tasks() {
    use cube_envd::server::{
        run_server_with_runtime, EnvdFailureKind, LifecycleState, ServerConfig, ServerError,
        ServerRuntime,
    };
    use std::sync::Arc;
    use std::time::Duration;
    let root = tempfile::Builder::new()
        .prefix("guest-supervision-")
        .tempdir_in("/sys/fs/cgroup")
        .unwrap();
    std::fs::write(root.path().join("cgroup.subtree_control"), "+cpu +memory").unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    drop(listener);
    let lifecycle = Arc::new(LifecycleState::new());
    let (logs, receiver) = tokio::sync::mpsc::unbounded_channel();
    let runtime = ServerRuntime::new(lifecycle.clone())
        .with_cgroup_root(root.path().to_str().unwrap().into())
        .with_guest_services(receiver);
    let server = tokio::spawn(run_server_with_runtime(
        ServerConfig {
            port,
            is_not_fc: false,
        },
        runtime,
    ));
    tokio::time::timeout(Duration::from_secs(5), async {
        while !lifecycle.is_ready() {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    assert_eq!(
        reqwest::get(format!("http://127.0.0.1:{port}/health"))
            .await
            .unwrap()
            .status(),
        204
    );
    drop(logs);
    let result = tokio::time::timeout(Duration::from_secs(2), server)
        .await
        .unwrap()
        .unwrap();
    let Err(ServerError::EnvdFailure(failure)) = result else {
        panic!("expected supervised guest failure");
    };
    assert_eq!(failure.kind(), EnvdFailureKind::UnexpectedCompletion);
    assert_eq!(failure.task(), "log exporter");
    assert!(!lifecycle.is_ready());
    assert!(tokio::net::TcpStream::connect(("127.0.0.1", port))
        .await
        .is_err());
    for class in ["socats", "user", "ptys"] {
        assert!(!root.path().join(class).exists());
    }
    // cgroup pseudo-files must not be traversed by TempDir's recursive removal.
    let path = root.keep();
    std::fs::remove_dir(path).unwrap();
}

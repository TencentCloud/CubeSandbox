// SPDX-License-Identifier: Apache-2.0

use std::process::Stdio;
use std::time::Duration;

use reqwest::{Client, StatusCode};
use serde_json::{json, Value};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::{Child, Command};
use tokio::time::timeout;

async fn start() -> (Child, String) {
    start_with(&[]).await
}

async fn start_with(extra: &[&str]) -> (Child, String) {
    let mut child = Command::new(env!("CARGO_BIN_EXE_cube-envd"))
        .args(["--port", "0", "--isnotfc", "--log-format", "json"])
        .args(extra)
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .kill_on_drop(true)
        .spawn()
        .expect("start daemon built by Cargo");
    let mut lines = BufReader::new(child.stdout.take().unwrap()).lines();
    let address = timeout(Duration::from_secs(15), async {
        while let Some(line) = lines.next_line().await.unwrap() {
            let event: Value = serde_json::from_str(&line).expect("structured daemon log");
            if event["fields"]["message"] == "envd listening" {
                return event["fields"]["addr"]
                    .as_str()
                    .unwrap()
                    .replace("0.0.0.0", "127.0.0.1");
            }
        }
        panic!("daemon exited before listening");
    })
    .await
    .expect("listening before timeout");
    // Keep draining stdout for the lifetime of the daemon.
    tokio::spawn(async move { while lines.next_line().await.ok().flatten().is_some() {} });
    (child, format!("http://{address}"))
}

async fn stop(mut child: Child, signal: i32) {
    assert_eq!(unsafe { libc::kill(child.id().unwrap() as i32, signal) }, 0);
    assert!(timeout(Duration::from_secs(5), child.wait())
        .await
        .expect("bounded shutdown")
        .unwrap()
        .success());
}

#[tokio::test]
async fn real_daemon_health_init_auth_cors_and_shutdown() {
    let (child, base) = start().await;
    let client = Client::new();
    let health = client.get(format!("{base}/health")).send().await.unwrap();
    assert_eq!(health.status(), StatusCode::NO_CONTENT);
    assert_eq!(health.headers()["cache-control"], "no-store");
    assert!(health.bytes().await.unwrap().is_empty());
    assert_eq!(
        client
            .head(format!("{base}/health"))
            .send()
            .await
            .unwrap()
            .status(),
        StatusCode::METHOD_NOT_ALLOWED
    );
    let init = client.post(format!("{base}/init"))
        .json(&json!({"envVars":{"C1_ENV":"visible"},"defaultUser":"root","accessToken":"test-secret"}))
        .send().await.unwrap();
    assert_eq!(init.status(), StatusCode::NO_CONTENT);
    assert_eq!(
        client
            .get(format!("{base}/health"))
            .send()
            .await
            .unwrap()
            .status(),
        StatusCode::NO_CONTENT
    );
    let denied = client
        .get(format!("{base}/envs"))
        .header("origin", "https://example.test")
        .send()
        .await
        .unwrap();
    assert_eq!(denied.status(), StatusCode::UNAUTHORIZED);
    assert_eq!(denied.headers()["access-control-allow-origin"], "*");
    assert_eq!(denied.json::<Value>().await.unwrap()["code"], 401);
    let envs = client
        .get(format!("{base}/envs"))
        .header("x-access-token", "test-secret")
        .send()
        .await
        .unwrap();
    assert_eq!(envs.status(), StatusCode::OK);
    let envs: Value = envs.json().await.unwrap();
    assert_eq!(envs["C1_ENV"], "visible");
    assert_eq!(envs["E2B_SANDBOX"], "false");
    let preflight = client
        .request(reqwest::Method::OPTIONS, format!("{base}/envs"))
        .header("origin", "https://example.test")
        .header("access-control-request-method", "GET")
        .header("access-control-request-headers", "X-Access-Token")
        .send()
        .await
        .unwrap();
    assert_eq!(preflight.status(), StatusCode::NO_CONTENT);
    assert_eq!(preflight.headers()["access-control-allow-methods"], "GET");
    assert_eq!(
        preflight.headers()["access-control-allow-headers"],
        "X-Access-Token"
    );
    assert_eq!(
        client
            .post(format!("{base}/init"))
            .json(&json!({"accessToken":"wrong"}))
            .send()
            .await
            .unwrap()
            .status(),
        StatusCode::UNAUTHORIZED
    );
    assert_eq!(
        client
            .post(format!("{base}/init"))
            .json(&json!({"accessToken":"test-secret","envVars":{"NEXT":"yes"}}))
            .send()
            .await
            .unwrap()
            .status(),
        StatusCode::NO_CONTENT
    );
    let listed = client
        .post(format!("{base}/process.Process/List"))
        .header("x-access-token", "test-secret")
        .json(&json!({}))
        .send()
        .await
        .unwrap();
    assert_eq!(listed.status(), StatusCode::OK);
    assert_eq!(listed.json::<Value>().await.unwrap(), json!({}));
    for path in [
        "/conformance.echo.Echo/Unary",
        "/__snapshot_test/runtime",
        "/filesystem.Filesystem/Stat",
        "/files",
    ] {
        let response = client
            .post(format!("{base}{path}"))
            .header("x-access-token", "test-secret")
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::NOT_FOUND, "{path}");
        assert_eq!(response.text().await.unwrap(), "404 page not found\n");
    }
    stop(child, libc::SIGTERM).await;
    assert!(client.get(format!("{base}/health")).send().await.is_err());
}

#[tokio::test]
async fn sigint_and_busy_port_have_distinct_exit_status() {
    let (child, base) = start().await;
    let port = base.rsplit(':').next().unwrap();
    let output = timeout(
        Duration::from_secs(15),
        Command::new(env!("CARGO_BIN_EXE_cube-envd"))
            .args(["--port", port, "--isnotfc"])
            .kill_on_drop(true)
            .output(),
    )
    .await
    .unwrap()
    .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("server I/O failure"));
    stop(child, libc::SIGINT).await;
}

#[test]
fn cli_identity_and_legacy_parser() {
    for args in [
        vec!["-version"],
        vec!["--version"],
        vec!["-port=0x_c3_3f", "-version"],
        vec!["-isnotfc=false", "-version"],
    ] {
        let output = std::process::Command::new(env!("CARGO_BIN_EXE_cube-envd"))
            .args(args)
            .output()
            .unwrap();
        assert!(output.status.success());
        assert_eq!(output.stdout, b"0.5.7\n");
    }
    for (arg, expected) in [
        ("--cube-version", cube_envd::PRODUCT_VERSION),
        ("-commit", cube_envd::REVISION),
    ] {
        let output = std::process::Command::new(env!("CARGO_BIN_EXE_cube-envd"))
            .arg(arg)
            .output()
            .unwrap();
        assert!(output.status.success());
        assert_eq!(String::from_utf8(output.stdout).unwrap().trim(), expected);
    }
    for port in ["0x", "1_", "08", "65536"] {
        let output = std::process::Command::new(env!("CARGO_BIN_EXE_cube-envd"))
            .args(["-port", port])
            .output()
            .unwrap();
        assert!(!output.status.success(), "{port}");
    }
}

#[tokio::test]
async fn non_cgroup_root_fails_before_listening_or_running_command() {
    let directory = tempfile::tempdir().unwrap();
    let marker = directory.path().join("ran");
    let output = Command::new(env!("CARGO_BIN_EXE_cube-envd"))
        .args(["-port", "0", "-isnotfc", "-cgroup-root"])
        .arg(directory.path())
        .arg("-cmd")
        .arg(format!("touch {}", marker.display()))
        .output()
        .await
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("not a cgroup v2 filesystem"));
    assert!(!marker.exists());
    assert!(!directory.path().join("user").exists());
}

#[tokio::test]
async fn startup_command_uses_the_managed_process_service() {
    let directory = tempfile::tempdir().unwrap();
    let marker = directory.path().join("startup");
    let command = format!("printf startup > {}; exec /bin/sleep 30", marker.display());
    let (child, base) = start_with(&["-cmd", &command, "-cgroup-root", "/sys/fs/cgroup"]).await;
    let client = Client::new();
    let listed: Value = client
        .post(format!("{base}/process.Process/List"))
        .json(&json!({}))
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    let process = &listed["processes"][0];
    assert_eq!(process["tag"], "startCmd");
    assert_eq!(process["config"]["cwd"], "/home/user");
    timeout(Duration::from_secs(5), async {
        while tokio::fs::read(&marker).await.unwrap_or_default() != b"startup" {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    assert_eq!(
        client
            .post(format!("{base}/process.Process/SendSignal"))
            .json(&json!({"process":{"pid":process["pid"]},"signal":9}))
            .send()
            .await
            .unwrap()
            .status(),
        StatusCode::OK
    );
    timeout(Duration::from_secs(5), async {
        loop {
            let listed: Value = client
                .post(format!("{base}/process.Process/List"))
                .json(&json!({}))
                .send()
                .await
                .unwrap()
                .json()
                .await
                .unwrap();
            if listed == json!({}) {
                break;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    stop(child, libc::SIGTERM).await;
}

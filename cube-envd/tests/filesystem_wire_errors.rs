// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

mod support;

use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, Stdio};
use std::time::Duration;

use buffa::Message;
use cube_envd::proto::filesystem::{
    ListDirRequest, ListDirResponse, MakeDirRequest, MakeDirResponse, MoveRequest, MoveResponse,
    RemoveRequest, RemoveResponse, StatRequest, StatResponse,
};
use serde_json::{json, Value};

async fn protobuf<Q: Message, R: Message + Default>(port: u16, method: &str, request: Q) -> R {
    let response = reqwest::Client::builder()
        .no_proxy()
        .build()
        .unwrap()
        .post(format!(
            "http://127.0.0.1:{port}/filesystem.Filesystem/{method}"
        ))
        .header("content-type", "application/proto")
        .header("connect-protocol-version", "1")
        .timeout(Duration::from_secs(5))
        .body(request.encode_to_vec())
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200, "{method}: {response:?}");
    assert_eq!(response.headers()["content-type"], "application/proto");
    R::decode(&mut response.bytes().await.unwrap().as_ref()).unwrap()
}

#[tokio::test]
async fn all_unary_methods_round_trip_raw_protobuf() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory
        .path()
        .join("created")
        .to_str()
        .unwrap()
        .to_owned();
    let destination = directory
        .path()
        .join("parents/moved")
        .to_str()
        .unwrap()
        .to_owned();
    let (port, server) = support::spawn_daemon().await;
    let made: MakeDirResponse = protobuf(
        port,
        "MakeDir",
        MakeDirRequest {
            path: path.clone(),
            ..Default::default()
        },
    )
    .await;
    assert_eq!(made.entry.path, path);
    assert_eq!(made.entry.mode, 0o755);
    let stated: StatResponse = protobuf(
        port,
        "Stat",
        StatRequest {
            path: path.clone(),
            ..Default::default()
        },
    )
    .await;
    assert_eq!(stated.entry, made.entry);
    let listed: ListDirResponse = protobuf(
        port,
        "ListDir",
        ListDirRequest {
            path: directory.path().to_str().unwrap().to_owned(),
            depth: 0,
            ..Default::default()
        },
    )
    .await;
    assert_eq!(listed.entries.len(), 1);
    assert_eq!(listed.entries[0].path, path);
    let moved: MoveResponse = protobuf(
        port,
        "Move",
        MoveRequest {
            source: path.clone(),
            destination: destination.clone(),
            ..Default::default()
        },
    )
    .await;
    assert_eq!(moved.entry.path, destination);
    assert!(!std::path::Path::new(&path).exists());
    assert!(std::path::Path::new(&destination).is_dir());
    for _ in 0..2 {
        let _: RemoveResponse = protobuf(
            port,
            "Remove",
            RemoveRequest {
                path: destination.clone(),
                ..Default::default()
            },
        )
        .await;
    }
    assert!(!std::path::Path::new(&destination).exists());
    server.abort();
}

struct Daemon(Child);
impl Drop for Daemon {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[tokio::test]
async fn real_eacces_maps_read_and_mutation_failures_without_daemon_failure() {
    // This acceptance test runs in the required privileged Rust container.
    assert_eq!(
        unsafe { libc::geteuid() },
        0,
        "requires root to launch an unprivileged daemon"
    );
    let directory = tempfile::tempdir().unwrap();
    std::fs::set_permissions(directory.path(), std::fs::Permissions::from_mode(0o755)).unwrap();
    // The cache containing Cargo's executable may not be traversable by nobody.
    let binary = directory.path().join("envd");
    std::fs::copy(env!("CARGO_BIN_EXE_cube-envd"), &binary).unwrap();
    std::fs::set_permissions(&binary, std::fs::Permissions::from_mode(0o755)).unwrap();
    let protected = directory.path().join("protected");
    std::fs::create_dir(&protected).unwrap();
    std::fs::set_permissions(&protected, std::fs::Permissions::from_mode(0o700)).unwrap();
    let hidden = protected.join("hidden");
    std::fs::write(&hidden, b"preserved").unwrap();
    let visible = directory.path().join("visible");
    std::fs::write(&visible, b"source").unwrap();
    let cgroup_root = std::path::Path::new("/sys/fs/cgroup")
        .join(format!("cube-envd-eacces-{}", std::process::id()));
    std::fs::create_dir(&cgroup_root).unwrap();
    std::fs::write(cgroup_root.join("cgroup.subtree_control"), "+cpu +memory").unwrap();
    for (class, properties) in [
        ("ptys", &["cpu.weight", "memory.high", "memory.max"][..]),
        ("socats", &["cpu.weight", "memory.min", "memory.low"][..]),
        ("user", &["cpu.weight", "memory.high", "memory.max"][..]),
    ] {
        let class = cgroup_root.join(class);
        std::fs::create_dir(&class).unwrap();
        for property in properties.iter().copied().chain(["cgroup.procs"]) {
            let path = class.join(property);
            let path = std::ffi::CString::new(path.as_os_str().as_bytes()).unwrap();
            assert_eq!(unsafe { libc::chown(path.as_ptr(), 65534, 65534) }, 0);
        }
    }
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    drop(listener);
    let mut daemon = Daemon(
        Command::new(&binary)
            .args([
                "--port",
                &port.to_string(),
                "--cgroup-root",
                cgroup_root.to_str().unwrap(),
            ])
            .uid(65534)
            .gid(65534)
            .current_dir(directory.path())
            .stdout(Stdio::null())
            .stderr(Stdio::inherit())
            .spawn()
            .unwrap(),
    );
    let client = reqwest::Client::builder()
        .no_proxy()
        .timeout(Duration::from_secs(5))
        .build()
        .unwrap();
    let health = format!("http://127.0.0.1:{port}/health");
    tokio::time::timeout(Duration::from_secs(10), async {
        loop {
            assert!(
                daemon.0.try_wait().unwrap().is_none(),
                "daemon exited during startup"
            );
            if let Ok(response) = client.get(&health).send().await {
                if response.status() == 204 {
                    break;
                }
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("unprivileged daemon did not become healthy");
    for (method, body, message) in [
        (
            "Stat",
            json!({"path": hidden}),
            format!(
                "error getting file info: lstat {}: permission denied",
                hidden.display()
            ),
        ),
        (
            "ListDir",
            json!({"path": protected}),
            format!(
                "error reading directory {0}: open {0}: permission denied",
                protected.display()
            ),
        ),
        (
            "MakeDir",
            json!({"path": protected.join("new/child")}),
            format!(
                "error getting file info: stat {}: permission denied",
                protected.join("new/child").display()
            ),
        ),
        (
            "Remove",
            json!({"path": hidden}),
            format!(
                "error removing file or directory: open {}: permission denied",
                protected.display()
            ),
        ),
        (
            "Move",
            json!({"source": hidden, "destination": visible}),
            format!(
                "error renaming: rename {} {}: permission denied",
                hidden.display(),
                visible.display()
            ),
        ),
        (
            "Move",
            json!({"source": visible, "destination": protected.join("new/child")}),
            format!(
                "failed to stat directory: stat {}: permission denied",
                protected.join("new").display()
            ),
        ),
        (
            "Remove",
            json!({"path": visible}),
            format!(
                "error removing file or directory: unlinkat {}: permission denied",
                visible.display()
            ),
        ),
    ] {
        let response = client
            .post(format!(
                "http://127.0.0.1:{port}/filesystem.Filesystem/{method}"
            ))
            .header("connect-protocol-version", "1")
            .json(&body)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 500, "{method}: {response:?}");
        let error: Value = response.json().await.unwrap();
        assert_eq!(
            error,
            json!({"code": "internal", "message": message}),
            "{method}"
        );
    }
    assert_eq!(std::fs::read(&hidden).unwrap(), b"preserved");
    assert_eq!(std::fs::read(&visible).unwrap(), b"source");
    assert!(!protected.join("new").exists());
    assert_eq!(client.get(health).send().await.unwrap().status(), 204);
    drop(daemon);
    for class in ["ptys", "socats", "user"] {
        std::fs::remove_dir(cgroup_root.join(class)).unwrap();
    }
    std::fs::remove_dir(cgroup_root).unwrap();
}

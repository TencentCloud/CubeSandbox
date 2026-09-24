// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

mod support;

use serde_json::{json, Value};

async fn rpc(port: u16, method: &str, body: Value) -> (reqwest::StatusCode, Value) {
    let response = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/filesystem.Filesystem/{method}"
        ))
        .header("connect-protocol-version", "1")
        .json(&body)
        .send()
        .await
        .unwrap();
    (response.status(), response.json().await.unwrap())
}

#[tokio::test]
async fn stat_reports_a_real_file_and_read_default_from_init() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("hello"), b"hello").unwrap();
    let (port, server) = support::spawn_daemon().await;
    let init = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/init"))
        .json(&json!({"defaultWorkdir": dir.path()}))
        .send()
        .await
        .unwrap();
    assert_eq!(init.status(), 204);
    let (status, body) = rpc(port, "Stat", json!({"path": dir.path().join("hello")})).await;
    assert_eq!(status, 200, "{body}");
    assert_eq!(body["entry"]["name"], "hello");
    assert_eq!(body["entry"]["type"], "FILE_TYPE_FILE");
    let (status, body) = rpc(port, "Stat", json!({"path": ""})).await;
    assert_eq!(status, 200, "{body}");
    assert_eq!(body["entry"]["path"], dir.path().to_str().unwrap());
    assert_eq!(body["entry"]["type"], "FILE_TYPE_DIRECTORY");
    server.abort();
}

#[tokio::test]
async fn stat_symlinks_report_target_type_and_mode_without_a_symlink_enum() {
    use std::os::unix::fs::{symlink, PermissionsExt};
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("target"), b"hello").unwrap();
    std::fs::set_permissions(
        dir.path().join("target"),
        std::fs::Permissions::from_mode(0o640),
    )
    .unwrap();
    symlink("target", dir.path().join("link")).unwrap();
    symlink("missing", dir.path().join("broken")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let (status, body) = rpc(port, "Stat", json!({"path": dir.path().join("link")})).await;
    assert_eq!(status, 200, "{body}");
    assert_eq!(body["entry"]["type"], "FILE_TYPE_FILE");
    assert_eq!(body["entry"]["mode"], 0o640);
    assert_eq!(body["entry"]["permissions"], "Lrwxrwxrwx");
    assert_eq!(body["entry"]["size"], "6");
    assert_eq!(
        body["entry"]["symlinkTarget"],
        dir.path().join("target").to_str().unwrap()
    );
    let (status, body) = rpc(port, "Stat", json!({"path": dir.path().join("broken")})).await;
    assert_eq!(status, 200, "{body}");
    assert!(body["entry"]["type"].is_null() || body["entry"]["type"] == "FILE_TYPE_UNSPECIFIED");
    assert!(body["entry"]["mode"].is_null() || body["entry"]["mode"] == 0);
    assert_eq!(
        body["entry"]["symlinkTarget"],
        dir.path().join("broken").to_str().unwrap()
    );
    server.abort();
}

#[tokio::test]
async fn mkdir_creates_owned_parents_and_preserves_existing_directory_semantics() {
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    let path = dir.path().join("new/child");
    let (status, body) = rpc(port, "MakeDir", json!({"path": path})).await;
    assert_eq!(status, 200, "{body}");
    for path in [dir.path().join("new"), path.clone()] {
        let (_, body) = rpc(port, "Stat", json!({"path": path})).await;
        assert_eq!(body["entry"]["mode"], 0o755);
        assert_eq!(body["entry"]["owner"], "root");
    }
    for path in [path.to_str().unwrap(), "/"] {
        let (_, body) = rpc(port, "MakeDir", json!({"path": path})).await;
        assert_eq!(body["code"], "already_exists", "{body}");
    }
    let (_, body) = rpc(port, "MakeDir", json!({"path": ""})).await;
    assert_eq!(body["code"], "already_exists");
    std::fs::write(dir.path().join("file"), b"x").unwrap();
    let (_, body) = rpc(port, "MakeDir", json!({"path": dir.path().join("file")})).await;
    assert_eq!(body["code"], "invalid_argument");
    server.abort();
}

#[tokio::test]
async fn move_creates_destination_parents_and_keeps_posix_collision_errors() {
    let dir = tempfile::tempdir().unwrap();
    let source = dir.path().join("source");
    std::fs::write(&source, b"moved").unwrap();
    let destination = dir.path().join("parents/child/file");
    let (port, server) = support::spawn_daemon().await;
    let (status, body) = rpc(
        port,
        "Move",
        json!({"source": source, "destination": destination}),
    )
    .await;
    assert_eq!(status, 200, "{body}");
    assert_eq!(body["entry"]["path"], destination.to_str().unwrap());
    assert_eq!(std::fs::read(&destination).unwrap(), b"moved");
    assert!(!source.exists());
    let (_, body) = rpc(port, "Stat", json!({"path": destination.parent().unwrap()})).await;
    assert_eq!(body["entry"]["mode"], 0o755);
    // Empty mutation paths resolve the configured default before root protection.
    let init = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/init"))
        .json(&json!({"defaultWorkdir": "/"}))
        .send()
        .await
        .unwrap();
    assert_eq!(init.status(), 204);
    for source in ["", "/", "/tmp/.."] {
        let (_, body) = rpc(
            port,
            "Move",
            json!({"source": source, "destination": destination}),
        )
        .await;
        assert_eq!(body["code"], "invalid_argument", "{body}");
    }
    for destination in ["", "/", "/tmp/.."] {
        let (_, body) = rpc(
            port,
            "Move",
            json!({"source": source, "destination": destination}),
        )
        .await;
        assert_eq!(body["code"], "invalid_argument", "{body}");
    }
    std::fs::create_dir(&source).unwrap();
    let (_, body) = rpc(
        port,
        "Move",
        json!({"source": source, "destination": dir.path().join("parents")}),
    )
    .await;
    assert_eq!(body["code"], "internal", "{body}");
    server.abort();
}

#[tokio::test]
async fn list_is_depth_first_ordered_and_does_not_recurse_into_child_symlinks() {
    use std::os::unix::fs::symlink;
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path().join("root");
    std::fs::create_dir_all(root.join("a/nested")).unwrap();
    std::fs::write(root.join("z"), b"z").unwrap();
    std::fs::write(root.join("a/file"), b"a").unwrap();
    symlink("a", root.join("link")).unwrap();
    symlink("missing", root.join("broken")).unwrap();
    symlink("root", dir.path().join("alias")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let (status, body) = rpc(port, "ListDir", json!({"path": root, "depth": 2})).await;
    assert_eq!(status, 200, "{body}");
    let names: Vec<_> = body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|entry| entry["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, ["a", "file", "nested", "broken", "link", "z"]);
    let (status, body) = rpc(
        port,
        "ListDir",
        json!({"path": dir.path().join("alias"), "depth": 0}),
    )
    .await;
    assert_eq!(status, 200, "{body}");
    assert_eq!(body["entries"].as_array().unwrap().len(), 4);
    assert!(body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .all(|entry| entry["path"]
            .as_str()
            .unwrap()
            .starts_with(dir.path().join("alias").to_str().unwrap())));
    let (status, body) = rpc(
        port,
        "ListDir",
        json!({"path": root, "depth": 4294967295u32}),
    )
    .await;
    assert_eq!(status, 200, "{body}");
    let names: Vec<_> = body["entries"]
        .as_array()
        .unwrap()
        .iter()
        .map(|entry| entry["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, ["a", "file", "nested", "broken", "link", "z"]);
    server.abort();
}

#[tokio::test]
async fn remove_is_missing_idempotent_and_never_recurses_through_final_symlink() {
    use std::os::unix::fs::symlink;
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path().join("root");
    std::fs::create_dir_all(root.join("child")).unwrap();
    std::fs::write(root.join("child/file"), b"keep").unwrap();
    symlink("root", dir.path().join("link")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let (status, body) = rpc(
        port,
        "Remove",
        json!({"path": format!("{}/link/", dir.path().display())}),
    )
    .await;
    assert_eq!(status, 200, "{body}");
    assert_eq!(std::fs::read(root.join("child/file")).unwrap(), b"keep");
    assert!(!dir.path().join("link").is_symlink());
    // Empty mutation paths still reject a default directory resolving to root.
    let init = reqwest::Client::new()
        .post(format!("http://127.0.0.1:{port}/init"))
        .json(&json!({"defaultWorkdir": "/"}))
        .send()
        .await
        .unwrap();
    assert_eq!(init.status(), 204);
    for path in ["", "/", "/tmp/../"] {
        let (_, body) = rpc(port, "Remove", json!({"path": path})).await;
        assert_eq!(body["code"], "invalid_argument");
    }
    for _ in 0..2 {
        let (status, body) = rpc(port, "Remove", json!({"path": root})).await;
        assert_eq!(status, 200, "{body}");
    }
    assert!(!root.exists());
    server.abort();
}

#[tokio::test]
async fn list_returns_all_entries_beyond_former_count_and_byte_budgets() {
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    for index in 0..10_001 {
        std::fs::write(dir.path().join(format!("f{index:05}")), b"").unwrap();
    }
    let (status, body) = rpc(port, "ListDir", json!({"path": dir.path()})).await;
    assert_eq!(status, 200);
    assert_eq!(body["entries"].as_array().unwrap().len(), 10_001);
    let bytes_dir = tempfile::tempdir().unwrap();
    for index in 0..2_000 {
        std::fs::write(
            bytes_dir
                .path()
                .join(format!("{index:05}{}", "x".repeat(240))),
            b"",
        )
        .unwrap();
    }
    let (status, body) = rpc(port, "ListDir", json!({"path": bytes_dir.path()})).await;
    assert_eq!(status, 200);
    assert_eq!(body["entries"].as_array().unwrap().len(), 2_000);
    server.abort();
}

#[tokio::test]
async fn move_dotdot_destination_reaches_posix_directory_collision() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir(dir.path().join("child")).unwrap();
    let source = dir.path().join("source");
    std::fs::write(&source, b"keep").unwrap();
    let (port, server) = support::spawn_daemon().await;
    let (_, body) = rpc(
        port,
        "Move",
        json!({
            "source": source,
            "destination": format!("{}/child/..", dir.path().display())
        }),
    )
    .await;
    assert_eq!(body["code"], "internal", "{body}");
    assert_eq!(std::fs::read(&source).unwrap(), b"keep");
    server.abort();
}

#[tokio::test]
async fn remove_absolute_final_dot_preserves_oracle_refusal_before_deleting_children() {
    let dir = tempfile::tempdir().unwrap();
    let child = dir.path().join("child");
    std::fs::create_dir(&child).unwrap();
    std::fs::write(child.join("keep"), b"keep").unwrap();
    let (port, server) = support::spawn_daemon().await;
    let (_, body) = rpc(
        port,
        "Remove",
        json!({"path": format!("{}/.", child.display())}),
    )
    .await;
    assert_eq!(body["code"], "internal", "{body}");
    assert_eq!(std::fs::read(child.join("keep")).unwrap(), b"keep");
    server.abort();
}

#[tokio::test]
async fn mkdir_missing_prefix_before_dotdot_is_a_successful_creation() {
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    let path = format!("{}/missing/nested/..", dir.path().display());
    let (status, body) = rpc(port, "MakeDir", json!({"path": path})).await;
    assert_eq!(status, 200, "{body}");
    assert!(dir.path().join("missing/nested").is_dir());
    server.abort();
}

#[tokio::test]
async fn trailing_slash_retains_stat_and_move_kernel_semantics() {
    use std::os::unix::fs::symlink;
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir(dir.path().join("target")).unwrap();
    symlink("target", dir.path().join("link")).unwrap();
    let link = format!("{}/link/", dir.path().display());
    let (port, server) = support::spawn_daemon().await;
    let (status, body) = rpc(port, "Stat", json!({"path": link})).await;
    assert_eq!(status, 200, "{body}");
    assert_eq!(body["entry"]["type"], "FILE_TYPE_DIRECTORY");
    assert!(body["entry"].get("symlinkTarget").is_none(), "{body}");
    let (_, body) = rpc(
        port,
        "Move",
        json!({"source": link, "destination": dir.path().join("moved")}),
    )
    .await;
    assert_eq!(body["code"], "internal", "{body}");
    assert!(dir.path().join("link").is_symlink());
    assert!(dir.path().join("target").is_dir());
    server.abort();
}

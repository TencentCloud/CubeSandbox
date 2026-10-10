// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

mod support;

use serde_json::{json, Value};
use std::path::Path;
use std::time::Duration;

struct Watch {
    response: reqwest::Response,
    bytes: Vec<u8>,
}

impl Watch {
    async fn open(port: u16, path: &Path, recursive: bool) -> Self {
        let payload = serde_json::to_vec(&json!({"path": path, "recursive": recursive})).unwrap();
        let mut body = vec![0];
        body.extend_from_slice(&(payload.len() as u32).to_be_bytes());
        body.extend(payload);
        let response = reqwest::Client::new()
            .post(format!(
                "http://127.0.0.1:{port}/filesystem.Filesystem/WatchDir"
            ))
            .header("content-type", "application/connect+json")
            .header("connect-protocol-version", "1")
            .body(body)
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 200);
        Self {
            response,
            bytes: vec![],
        }
    }

    async fn next(&mut self) -> (u8, Value) {
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                if self.bytes.len() >= 5 {
                    let len = u32::from_be_bytes(self.bytes[1..5].try_into().unwrap()) as usize;
                    if self.bytes.len() >= len + 5 {
                        let result = (
                            self.bytes[0],
                            serde_json::from_slice(&self.bytes[5..5 + len]).unwrap(),
                        );
                        self.bytes.drain(..5 + len);
                        return result;
                    }
                }
                self.bytes
                    .extend(self.response.chunk().await.unwrap().expect("stream event"));
            }
        })
        .await
        .expect("watch progress")
    }

    async fn event(&mut self, name: &str, kind: &str) {
        assert_eq!(
            self.next().await,
            (0, json!({"filesystem":{"name":name,"type":kind}}))
        );
    }
}

#[tokio::test]
async fn start_then_ordered_create_write_remove_over_public_stream() {
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Watch::open(port, dir.path(), false).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    std::fs::write(dir.path().join("file"), b"content").unwrap();
    watch.event("file", "EVENT_TYPE_CREATE").await;
    watch.event("file", "EVENT_TYPE_WRITE").await;
    std::fs::remove_file(dir.path().join("file")).unwrap();
    watch.event("file", "EVENT_TYPE_REMOVE").await;
    server.abort();
}

#[tokio::test]
async fn recursive_existing_dynamic_and_populated_move_in_cover_post_observed_mutations() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(dir.path().join("old/deep")).unwrap();
    let outside = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(outside.path().join("incoming/deep")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Watch::open(port, dir.path(), true).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    std::fs::write(dir.path().join("old/deep/a"), b"a").unwrap();
    watch.event("old/deep/a", "EVENT_TYPE_CREATE").await;
    watch.event("old/deep/a", "EVENT_TYPE_WRITE").await;
    std::fs::create_dir(dir.path().join("new")).unwrap();
    watch.event("new", "EVENT_TYPE_CREATE").await;
    // The observed parent event, not a sleep, is the coverage barrier.
    std::fs::write(dir.path().join("new/b"), b"b").unwrap();
    watch.event("new/b", "EVENT_TYPE_CREATE").await;
    watch.event("new/b", "EVENT_TYPE_WRITE").await;
    std::fs::rename(outside.path().join("incoming"), dir.path().join("incoming")).unwrap();
    watch.event("incoming", "EVENT_TYPE_CREATE").await;
    watch.event("incoming/deep", "EVENT_TYPE_CREATE").await;
    std::fs::write(dir.path().join("incoming/deep/c"), b"c").unwrap();
    watch.event("incoming/deep/c", "EVENT_TYPE_CREATE").await;
    watch.event("incoming/deep/c", "EVENT_TYPE_WRITE").await;
    server.abort();
}

#[tokio::test]
async fn root_rename_detaches_original_and_ignores_replacement_and_symlink_retarget() {
    let dir = tempfile::tempdir().unwrap();
    let original = dir.path().join("root");
    let renamed = dir.path().join("renamed");
    let link = dir.path().join("link");
    std::fs::create_dir(&original).unwrap();
    std::os::unix::fs::symlink(&original, &link).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Watch::open(port, &link, false).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    std::fs::rename(&original, &renamed).unwrap();
    watch.event(".", "EVENT_TYPE_RENAME").await;
    std::fs::create_dir(&original).unwrap();
    std::fs::remove_file(&link).unwrap();
    std::os::unix::fs::symlink(&original, &link).unwrap();
    std::fs::write(original.join("wrong"), b"wrong").unwrap();
    std::fs::write(renamed.join("right"), b"right").unwrap();
    assert!(
        tokio::time::timeout(Duration::from_millis(100), watch.next())
            .await
            .is_err(),
        "renamed and replacement roots must both be unwatched"
    );
    server.abort();
}

#[tokio::test]
async fn subtree_rename_uses_new_path_and_move_out_preserves_older_prefix() {
    let dir = tempfile::tempdir().unwrap();
    let outside = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(dir.path().join("before/deep")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Watch::open(port, dir.path(), true).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    std::fs::rename(dir.path().join("before"), dir.path().join("after")).unwrap();
    watch.event("before", "EVENT_TYPE_RENAME").await;
    watch.event("after", "EVENT_TYPE_CREATE").await;
    std::fs::write(dir.path().join("after/deep/older"), b"old").unwrap();
    std::fs::rename(dir.path().join("after"), outside.path().join("out")).unwrap();
    watch.event("after/deep/older", "EVENT_TYPE_CREATE").await;
    watch.event("after/deep/older", "EVENT_TYPE_WRITE").await;
    watch.event("after", "EVENT_TYPE_RENAME").await;
    // Observed move-out is the removal barrier. A later root event fences off
    // the absence assertion without sleeping or assuming thread scheduling.
    std::fs::write(outside.path().join("out/deep/unwatched"), b"out").unwrap();
    std::fs::write(dir.path().join("barrier"), b"barrier").unwrap();
    watch.event("barrier", "EVENT_TYPE_CREATE").await;
    watch.event("barrier", "EVENT_TYPE_WRITE").await;
    server.abort();
}

#[tokio::test]
async fn root_delete_reports_remove_and_shutdown_cancels() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path().join("root");
    std::fs::create_dir(&root).unwrap();
    let (port, lifecycle, server) =
        support::spawn_envd(cube_envd::server::ServerPhase::Ready).await;
    let mut watch = Watch::open(port, &root, true).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    std::fs::write(root.join("prefix"), b"p").unwrap();
    watch.event("prefix", "EVENT_TYPE_CREATE").await;
    std::fs::remove_file(root.join("prefix")).unwrap();
    std::fs::remove_dir(&root).unwrap();
    watch.event("prefix", "EVENT_TYPE_WRITE").await;
    watch.event("prefix", "EVENT_TYPE_REMOVE").await;
    watch.event(".", "EVENT_TYPE_REMOVE").await;
    let mut watch = Watch::open(port, dir.path(), false).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    lifecycle.try_transition(cube_envd::server::ServerPhase::Draining);
    let (flag, body) = watch.next().await;
    assert_eq!(flag, 2);
    assert_eq!(body["error"]["code"], "canceled");
    server.abort();
}

#[tokio::test]
async fn deleted_children_release_recursive_watches() {
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Watch::open(port, dir.path(), true).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    for index in 0..1030 {
        let name = format!("child-{index}");
        std::fs::create_dir(dir.path().join(&name)).unwrap();
        watch.event(&name, "EVENT_TYPE_CREATE").await;
        std::fs::remove_dir(dir.path().join(&name)).unwrap();
        watch.event(&name, "EVENT_TYPE_REMOVE").await;
    }
    server.abort();
}

#[tokio::test]
async fn initial_recursive_tree_beyond_former_capacity_is_fully_watched() {
    let dir = tempfile::tempdir().unwrap();
    for index in 0..256 {
        std::fs::create_dir(dir.path().join(format!("child-{index}"))).unwrap();
    }
    let (port, server) = support::spawn_daemon().await;
    for _ in 0..3 {
        let mut watch = Watch::open(port, dir.path(), true).await;
        assert_eq!(watch.next().await, (0, json!({"start":{}})));
        std::fs::write(dir.path().join("child-255/file"), b"complete").unwrap();
        watch.event("child-255/file", "EVENT_TYPE_CREATE").await;
        watch.event("child-255/file", "EVENT_TYPE_WRITE").await;
        std::fs::remove_file(dir.path().join("child-255/file")).unwrap();
        watch.event("child-255/file", "EVENT_TYPE_REMOVE").await;
    }
    let mut watch = Watch::open(port, dir.path(), false).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    server.abort();
}

#[tokio::test]
async fn rapid_subtree_renames_do_not_reopen_obsolete_names() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(dir.path().join("a/deep")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Watch::open(port, dir.path(), true).await;
    assert_eq!(watch.next().await, (0, json!({"start":{}})));
    std::fs::rename(dir.path().join("a"), dir.path().join("b")).unwrap();
    std::fs::rename(dir.path().join("b"), dir.path().join("c")).unwrap();
    watch.event("a", "EVENT_TYPE_RENAME").await;
    watch.event("b", "EVENT_TYPE_CREATE").await;
    watch.event("b", "EVENT_TYPE_RENAME").await;
    watch.event("c", "EVENT_TYPE_CREATE").await;
    std::fs::write(dir.path().join("c/deep/file"), b"covered").unwrap();
    watch.event("c/deep/file", "EVENT_TYPE_CREATE").await;
    watch.event("c/deep/file", "EVENT_TYPE_WRITE").await;
    server.abort();
}

#[tokio::test]
async fn invalid_watch_creation_never_starts() {
    let dir = tempfile::tempdir().unwrap();
    let file = dir.path().join("file");
    std::fs::write(&file, b"file").unwrap();
    let (port, server) = support::spawn_daemon().await;
    for (path, code) in [
        (file, "invalid_argument"),
        (dir.path().join("missing"), "not_found"),
        (dir.path().join("nul\0path"), "internal"),
    ] {
        let mut stream = Watch::open(port, &path, true).await;
        let (flag, body) = stream.next().await;
        assert_eq!(flag, 2);
        assert_eq!(body["error"]["code"], code);
    }
    server.abort();
}

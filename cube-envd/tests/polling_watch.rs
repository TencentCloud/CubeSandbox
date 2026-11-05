// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

mod support;

use serde_json::{json, Value};
use std::time::Duration;

async fn rpc(port: u16, method: &str, body: Value) -> Value {
    reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/filesystem.Filesystem/{method}"
        ))
        .header("connect-protocol-version", "1")
        .json(&body)
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap()
}

#[tokio::test]
async fn polling_survives_create_connection_drains_and_removes() {
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    let created = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
    let id = created["watcherId"]
        .as_str()
        .expect("successful persistent creation");
    assert!(id.starts_with('w'));
    let query = json!({"watcherId":id});
    let empty = rpc(port, "GetWatcherEvents", query.clone()).await;
    assert!(empty.get("code").is_none(), "{empty}");
    assert!(empty["events"].as_array().is_none_or(Vec::is_empty));
    std::fs::write(dir.path().join("file"), b"content").unwrap();
    let events = tokio::time::timeout(Duration::from_secs(5), async {
        let mut events = vec![];
        while events.len() < 2 {
            let batch = rpc(port, "GetWatcherEvents", query.clone()).await;
            assert!(batch.get("code").is_none(), "{batch}");
            if let Some(batch) = batch["events"].as_array() {
                events.extend(batch.clone());
            }
            tokio::task::yield_now().await;
        }
        events
    })
    .await
    .unwrap();
    assert_eq!(
        events,
        vec![
            json!({"name":"file","type":"EVENT_TYPE_CREATE"}),
            json!({"name":"file","type":"EVENT_TYPE_WRITE"})
        ]
    );
    assert_eq!(rpc(port, "RemoveWatcher", query.clone()).await, json!({}));
    for method in ["GetWatcherEvents", "RemoveWatcher"] {
        assert_eq!(rpc(port, method, query.clone()).await["code"], "not_found");
        assert_eq!(rpc(port, method, json!({})).await["code"], "not_found");
    }
    server.abort();
}

struct Poll {
    port: u16,
    id: Value,
    pending: std::collections::VecDeque<Value>,
    error: Option<Value>,
}
impl Poll {
    async fn open(port: u16, path: &std::path::Path, recursive: bool) -> Self {
        let response = rpc(
            port,
            "CreateWatcher",
            json!({"path":path,"recursive":recursive}),
        )
        .await;
        Self {
            port,
            id: json!({"watcherId":response["watcherId"]}),
            pending: Default::default(),
            error: response.get("code").map(|_| response.clone()),
        }
    }
    async fn next(&mut self) -> (u8, Value) {
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                if let Some(event) = self.pending.pop_front() {
                    return (0, json!({"filesystem":event}));
                }
                if let Some(error) = &self.error {
                    return (2, json!({"error":error}));
                }
                let response = rpc(self.port, "GetWatcherEvents", self.id.clone()).await;
                if response.get("code").is_some() {
                    self.error = Some(response);
                } else if let Some(events) = response["events"].as_array() {
                    self.pending.extend(events.clone());
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("polling progress")
    }
    async fn event(&mut self, name: &str, kind: &str) {
        assert_eq!(
            self.next().await,
            (0, json!({"filesystem":{"name":name,"type":kind}}))
        );
    }
}
#[tokio::test]
async fn ordered_create_write_remove_over_public_polling() {
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Poll::open(port, dir.path(), false).await;
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
    let mut watch = Poll::open(port, dir.path(), true).await;
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
    let mut watch = Poll::open(port, &link, false).await;
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
    let mut watch = Poll::open(port, dir.path(), true).await;
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
    let mut watch = Poll::open(port, &root, true).await;
    std::fs::write(root.join("prefix"), b"p").unwrap();
    watch.event("prefix", "EVENT_TYPE_CREATE").await;
    std::fs::remove_file(root.join("prefix")).unwrap();
    std::fs::remove_dir(&root).unwrap();
    watch.event("prefix", "EVENT_TYPE_WRITE").await;
    watch.event("prefix", "EVENT_TYPE_REMOVE").await;
    watch.event(".", "EVENT_TYPE_REMOVE").await;
    let watch = Poll::open(port, dir.path(), false).await;
    assert!(watch.error.is_none());
    lifecycle.try_transition(cube_envd::server::ServerPhase::Draining);
    let response = reqwest::Client::new()
        .post(format!(
            "http://127.0.0.1:{port}/filesystem.Filesystem/GetWatcherEvents"
        ))
        .header("connect-protocol-version", "1")
        .json(&watch.id)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 503);
    use std::os::unix::fs::MetadataExt;
    cleaned(std::process::id(), dir.path().metadata().unwrap().ino()).await;
    server.abort();
}

#[tokio::test]
async fn deleted_children_release_recursive_watches() {
    let dir = tempfile::tempdir().unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Poll::open(port, dir.path(), true).await;
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
        let mut watch = Poll::open(port, dir.path(), true).await;
        std::fs::write(dir.path().join("child-255/file"), b"complete").unwrap();
        watch.event("child-255/file", "EVENT_TYPE_CREATE").await;
        watch.event("child-255/file", "EVENT_TYPE_WRITE").await;
        std::fs::remove_file(dir.path().join("child-255/file")).unwrap();
        watch.event("child-255/file", "EVENT_TYPE_REMOVE").await;
    }
    let watch = Poll::open(port, dir.path(), false).await;
    assert!(watch.error.is_none());
    server.abort();
}

#[tokio::test]
async fn rapid_subtree_renames_do_not_reopen_obsolete_names() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(dir.path().join("a/deep")).unwrap();
    let (port, server) = support::spawn_daemon().await;
    let mut watch = Poll::open(port, dir.path(), true).await;
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

fn root_is_watched(pid: u32, root: &std::path::Path) -> bool {
    use std::os::unix::fs::MetadataExt;
    let inode = std::fs::metadata(root).unwrap().ino();
    inode_is_watched(pid, inode)
}

fn inode_is_watched(pid: u32, inode: u64) -> bool {
    std::fs::read_dir(format!("/proc/{pid}/fdinfo"))
        .unwrap()
        .filter_map(Result::ok)
        .any(|item| {
            std::fs::read_to_string(item.path())
                .unwrap_or_default()
                .lines()
                .any(|line| {
                    line.starts_with("inotify ")
                        && line
                            .split_whitespace()
                            .any(|field| field == format!("ino:{inode:x}"))
                })
        })
}

async fn cleaned(pid: u32, inode: u64) {
    tokio::time::timeout(Duration::from_secs(5), async {
        while inode_is_watched(pid, inode) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("watch resources released");
}

#[tokio::test]
async fn event_burst_is_retained_until_read_and_remove_releases_resources() {
    use std::os::unix::fs::MetadataExt;
    let dir = tempfile::tempdir().unwrap();
    let inode = dir.path().metadata().unwrap().ino();
    let (port, daemon_pid, server) = support::spawn_daemon_with_pid().await;
    let created = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
    let query = json!({"watcherId":created["watcherId"]});
    assert!(root_is_watched(daemon_pid, dir.path()));
    for i in 0..300 {
        std::fs::create_dir(dir.path().join(format!("event-{i}"))).unwrap();
    }
    let mut names = std::collections::HashSet::new();
    tokio::time::timeout(Duration::from_secs(5), async {
        while names.len() < 300 {
            let response = rpc(port, "GetWatcherEvents", query.clone()).await;
            assert!(response.get("code").is_none(), "{response}");
            for event in response["events"].as_array().into_iter().flatten() {
                assert_eq!(event["type"], "EVENT_TYPE_CREATE");
                assert!(names.insert(event["name"].as_str().unwrap().to_owned()));
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    assert_eq!(names, (0..300).map(|i| format!("event-{i}")).collect());
    assert!(root_is_watched(daemon_pid, dir.path()));
    assert_eq!(rpc(port, "RemoveWatcher", query.clone()).await, json!({}));
    assert_eq!(rpc(port, "RemoveWatcher", query).await["code"], "not_found");
    cleaned(daemon_pid, inode).await;
    server.abort();
}

#[tokio::test]
async fn root_removal_keeps_events_after_backend_cleanup_and_remove_succeeds() {
    use std::os::unix::fs::MetadataExt;
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path().join("root");
    std::fs::create_dir(&root).unwrap();
    let inode = root.metadata().unwrap().ino();
    let (port, daemon_pid, server) = support::spawn_daemon_with_pid().await;
    let created = rpc(port, "CreateWatcher", json!({"path":root})).await;
    let query = json!({"watcherId":created["watcherId"]});
    std::fs::create_dir(root.join("prefix")).unwrap();
    std::fs::remove_dir(root.join("prefix")).unwrap();
    std::fs::remove_dir(&root).unwrap();
    cleaned(daemon_pid, inode).await;
    let response = rpc(port, "GetWatcherEvents", query.clone()).await;
    assert_eq!(
        response["events"],
        json!([{"name":"prefix","type":"EVENT_TYPE_CREATE"},{"name":"prefix","type":"EVENT_TYPE_REMOVE"},{"name":".","type":"EVENT_TYPE_REMOVE"}])
    );
    for _ in 0..2 {
        assert_eq!(
            rpc(port, "GetWatcherEvents", query.clone()).await,
            json!({})
        );
    }
    assert_eq!(rpc(port, "RemoveWatcher", query).await, json!({}));
    server.abort();
}

#[tokio::test]
async fn active_watchers_beyond_former_capacity_are_not_evicted_and_remove_reclaims_resources() {
    let dir = tempfile::tempdir().unwrap();
    let (port, daemon_pid, server) = support::spawn_daemon_with_pid().await;
    let mut ids = std::collections::HashSet::new();
    for _ in 0..72 {
        let response = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
        assert!(ids.insert(response["watcherId"].as_str().expect("create").to_owned()));
    }
    for id in &ids {
        assert!(rpc(port, "GetWatcherEvents", json!({"watcherId":id}))
            .await
            .get("code")
            .is_none());
        assert_eq!(
            rpc(port, "RemoveWatcher", json!({"watcherId":id})).await,
            json!({})
        );
    }
    assert!(!root_is_watched(daemon_pid, dir.path()));
    let next = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
    assert!(!ids.contains(next["watcherId"].as_str().unwrap()));
    assert_eq!(
        rpc(
            port,
            "RemoveWatcher",
            json!({"watcherId":next["watcherId"]})
        )
        .await,
        json!({})
    );
    server.abort();
}

#[tokio::test]
async fn concurrent_gets_return_disjoint_ordered_prefixes_without_loss() {
    let dir = tempfile::tempdir().unwrap();
    let (port, daemon_pid, server) = support::spawn_daemon_with_pid().await;
    let created = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
    let query = json!({"watcherId":created["watcherId"]});
    for i in 0..100 {
        std::fs::create_dir(dir.path().join(format!("event-{i}"))).unwrap();
    }
    let mut seen = std::collections::HashSet::new();
    tokio::time::timeout(Duration::from_secs(5), async {
        while seen.len() < 100 {
            let barrier = std::sync::Arc::new(tokio::sync::Barrier::new(3));
            let mut tasks = vec![];
            for _ in 0..2 {
                let barrier = barrier.clone();
                let query = query.clone();
                tasks.push(tokio::spawn(async move {
                    barrier.wait().await;
                    rpc(port, "GetWatcherEvents", query).await
                }));
            }
            barrier.wait().await;
            for task in tasks {
                let batch = task.await.unwrap();
                assert!(batch.get("code").is_none());
                let mut previous = None;
                for event in batch["events"].as_array().into_iter().flatten() {
                    assert_eq!(event["type"], "EVENT_TYPE_CREATE");
                    let index: usize = event["name"]
                        .as_str()
                        .unwrap()
                        .strip_prefix("event-")
                        .unwrap()
                        .parse()
                        .unwrap();
                    if let Some(previous) = previous {
                        assert_eq!(index, previous + 1, "complete ordered prefix");
                    }
                    previous = Some(index);
                    assert!(seen.insert(index), "duplicate {index}");
                }
            }
        }
    })
    .await
    .unwrap();
    assert_eq!(seen, (0..100).collect());
    assert_eq!(rpc(port, "RemoveWatcher", query.clone()).await, json!({}));
    std::fs::create_dir(dir.path().join("after-remove")).unwrap();
    assert_eq!(
        rpc(port, "GetWatcherEvents", query).await["code"],
        "not_found"
    );
    assert!(!root_is_watched(daemon_pid, dir.path()));
    server.abort();
}

#[tokio::test]
async fn removed_roots_do_not_evict_older_ids_or_healthy_registry_entries() {
    use std::os::unix::fs::MetadataExt;
    let dir = tempfile::tempdir().unwrap();
    let (port, daemon_pid, server) = support::spawn_daemon_with_pid().await;
    let healthy_root = dir.path().join("healthy");
    std::fs::create_dir(&healthy_root).unwrap();
    let healthy = rpc(port, "CreateWatcher", json!({"path":healthy_root})).await;
    let mut ids = vec![];
    // Each unregistered root still owns its inotify instance, as upstream does.
    // Stay below the kernel max_user_instances while checking ID retention.
    for index in 0..16 {
        let root = dir.path().join(format!("terminal-{index}"));
        std::fs::create_dir(&root).unwrap();
        let inode = root.metadata().unwrap().ino();
        let created = rpc(port, "CreateWatcher", json!({"path":root})).await;
        assert!(created["watcherId"].is_string(), "{created}");
        ids.push(json!({"watcherId":created["watcherId"]}));
        std::fs::remove_dir(&root).unwrap();
        cleaned(daemon_pid, inode).await;
        // Kernel cleanup can precede the worker enqueueing DELETE_SELF.
        let events = tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let batch = rpc(port, "GetWatcherEvents", ids.last().unwrap().clone()).await;
                assert!(batch.get("code").is_none(), "{batch}");
                if batch["events"]
                    .as_array()
                    .is_some_and(|events| !events.is_empty())
                {
                    return batch["events"].clone();
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("root removal event queued");
        assert_eq!(events, json!([{"name":".","type":"EVENT_TYPE_REMOVE"}]));
    }
    for id in &ids {
        assert_eq!(rpc(port, "GetWatcherEvents", id.clone()).await, json!({}));
        assert_eq!(rpc(port, "RemoveWatcher", id.clone()).await, json!({}));
    }
    let query = json!({"watcherId":healthy["watcherId"]});
    assert!(rpc(port, "GetWatcherEvents", query.clone())
        .await
        .get("code")
        .is_none());
    assert_eq!(rpc(port, "RemoveWatcher", query).await, json!({}));
    server.abort();
}

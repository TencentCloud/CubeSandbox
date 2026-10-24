// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use super::*;
use crate::proto::filesystem::{CreateWatcherRequest, GetWatcherEventsResponse};
use std::sync::atomic::AtomicU64;
use std::sync::Mutex;

#[derive(Default)]
pub(super) struct Queue {
    events: Vec<FilesystemEvent>,
    terminal: Option<(Instant, DomainError)>,
    pub(super) removed: bool,
}

impl Queue {
    pub(super) fn append(&mut self, event: FilesystemEvent) -> Result<(), DomainError> {
        if self.removed {
            return Err(DomainError::Cancelled);
        }
        if let Some((_, error)) = &self.terminal {
            return Err(error.clone());
        }
        self.events.push(event);
        Ok(())
    }

    fn finish(&mut self, error: DomainError) {
        if self.removed || self.terminal.is_some() {
            return;
        }
        self.terminal = Some((Instant::now(), error));
    }

    fn drain(&mut self) -> Result<GetWatcherEventsResponse, DomainError> {
        if let Some((_, error)) = &self.terminal {
            return Err(error.clone());
        }
        Ok(GetWatcherEventsResponse {
            events: std::mem::take(&mut self.events),
            ..Default::default()
        })
    }
}

struct Watcher {
    queue: Arc<Mutex<Queue>>,
    cancel: CancelOnDrop,
    done: tokio::sync::watch::Receiver<bool>,
}

pub(crate) struct Registry {
    entries: Mutex<HashMap<String, Watcher>>,
    reaper: Mutex<Option<tokio::task::AbortHandle>>,
    #[cfg(test)]
    id_candidates: Mutex<VecDeque<String>>,
}

impl Default for Registry {
    fn default() -> Self {
        Self {
            entries: Mutex::new(HashMap::new()),
            reaper: Mutex::new(None),
            #[cfg(test)]
            id_candidates: Mutex::new(VecDeque::new()),
        }
    }
}

impl Registry {
    pub(crate) fn self_check(&self) -> bool {
        self.reaper
            .lock()
            .is_ok_and(|reaper| reaper.as_ref().is_none_or(|task| !task.is_finished()))
            && self.entries.lock().is_ok_and(|entries| {
                entries
                    .values()
                    .all(|watcher| watcher.queue.lock().is_ok_and(|queue| !queue.removed))
            })
    }

    fn next_id(&self) -> Result<String, DomainError> {
        #[cfg(test)]
        if let Some(id) = self.id_candidates.lock().unwrap().pop_front() {
            return Ok(id);
        }
        next_id()
    }

    fn start_reaper(self: &Arc<Self>, lifecycle: Arc<LifecycleState>) {
        let mut reaper = self.reaper.lock().unwrap();
        if reaper.is_some() {
            return;
        }
        let weak = Arc::downgrade(self);
        let task = tokio::spawn(async move {
            loop {
                tokio::time::sleep(Duration::from_millis(100)).await;
                let Some(registry) = weak.upgrade() else {
                    return;
                };
                let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    if matches!(
                        lifecycle.phase(),
                        ServerPhase::Draining | ServerPhase::Stopped | ServerPhase::Failed
                    ) {
                        registry.entries.lock().unwrap().clear();
                        true
                    } else {
                        false
                    }
                }));
                match result {
                    Ok(false) => {}
                    Ok(true) => return,
                    Err(_) => {
                        lifecycle.fail("polling watcher registry panic");
                        return;
                    }
                }
            }
        });
        *reaper = Some(task.abort_handle());
    }
}

fn missing(id: &str) -> DomainError {
    DomainError::NotFound(format!("watcher with id {id} not found"))
}

fn next_id() -> Result<String, DomainError> {
    // Entropy prevents guessing; a non-wrapping sequence prevents normal-time
    // reuse even if the OS happens to return the same random bytes again.
    static SEQUENCE: AtomicU64 = AtomicU64::new(0);
    let sequence = SEQUENCE
        .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |n| n.checked_add(1))
        .map_err(|_| exhausted())?;
    let mut random = [0u8; 16];
    let mut offset = 0;
    while offset < random.len() {
        let count = unsafe {
            libc::getrandom(
                random[offset..].as_mut_ptr().cast(),
                random.len() - offset,
                libc::GRND_NONBLOCK,
            )
        };
        if count < 0 {
            let error = std::io::Error::last_os_error();
            if error.kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            return Err(exhausted());
        }
        if count == 0 {
            return Err(DomainError::Internal);
        }
        offset += count as usize;
    }
    use base64::Engine;
    Ok(format!(
        "w{}-{sequence:x}",
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(random)
    ))
}

impl FilesystemService {
    #[cfg(test)]
    pub(crate) fn watcher_snapshot_observation(&self) -> serde_json::Value {
        // No reap or drain: observing a capture point must not alter its queue
        // or hide evidence that the background reaper ran without a request.
        let entries = self.polling.entries.lock().unwrap();
        let watchers: serde_json::Map<String, serde_json::Value> = entries
            .iter()
            .map(|(id, watcher)| {
                let queue = watcher.queue.lock().unwrap();
                let events: Vec<_> = queue.events.iter().map(|event| {
                    serde_json::json!({"name": event.name, "type": event.r#type})
                }).collect();
                (id.clone(), serde_json::json!({
                    "events": events,
                    "terminal": queue.terminal.as_ref().map(|(_, error)| format!("{:?}", error.connect_code())),
                    "terminalAge": queue.terminal.as_ref().map(|(at, _)| at.elapsed().as_secs_f64()),
                }))
            })
            .collect();
        serde_json::json!({"watchers": watchers})
    }

    pub(crate) async fn create_watcher(
        &self,
        request: CreateWatcherRequest,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
        lifecycle: Arc<LifecycleState>,
    ) -> Result<String, DomainError> {
        #[cfg(test)]
        let capture_tag = Path::new(&request.path)
            .file_name()
            .and_then(|name| name.to_str())
            .map(str::to_owned);
        #[cfg(test)]
        let _attempt =
            crate::filesystem::snapshot_test::attempt_at("WatcherCreate", capture_tag.as_deref());
        self.polling.start_reaper(lifecycle.clone());
        let queue = Arc::new(Mutex::new(Queue::default()));
        let send = Sink::Polling(queue.clone());
        let mut backend = self
            .prepare_watch(
                WatchDirRequest {
                    path: request.path,
                    recursive: request.recursive,
                    ..Default::default()
                },
                headers,
                snapshot,
                users,
            )
            .await?;
        let cancel = CancelOnDrop(Arc::new(AtomicBool::new(false)));
        let stopped = cancel.0.clone();
        let (done, receive) = tokio::sync::watch::channel(false);
        let worker_queue = queue.clone();
        let worker_lifecycle = lifecycle.clone();
        std::thread::Builder::new()
            .name("envd-poll-watch".into())
            .spawn(move || {
                let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    backend.run(&send, &stopped, &worker_lifecycle)
                }));
                let error = result.unwrap_or_else(|_| {
                    worker_lifecycle.fail("polling watch worker panic");
                    DomainError::Internal
                });
                worker_queue.lock().unwrap().finish(error);
                drop(backend);
                let _ = done.send(true);
            })
            .map_err(|_| exhausted())?;

        #[cfg(test)]
        crate::filesystem::snapshot_test::barrier("WatcherBeforePublish", capture_tag.as_deref())
            .await?;

        // Production has no await between establishing the producer and publishing. Before
        // this commit, cancellation drops the prepared backend or cancel guard.
        let mut entries = self.polling.entries.lock().unwrap();
        let state = queue.lock().unwrap();
        if let Some((_, error)) = &state.terminal {
            return Err(error.clone());
        }
        if lifecycle.phase() != ServerPhase::Ready {
            return Err(DomainError::Unavailable);
        }
        for _ in 0..8 {
            let id = self.polling.next_id()?;
            if entries.contains_key(&id) {
                continue;
            }
            drop(state);
            entries.insert(
                id.clone(),
                Watcher {
                    queue,
                    cancel,
                    done: receive,
                },
            );
            return Ok(id);
        }
        Err(exhausted())
    }

    pub(crate) fn get_watcher_events(
        &self,
        id: &str,
    ) -> Result<GetWatcherEventsResponse, DomainError> {
        // Hold lookup through the queue exchange. Remove takes the same locks
        // in this order; append takes only the queue lock and never blocks on I/O.
        let entries = self.polling.entries.lock().unwrap();
        let result = entries
            .get(id)
            .ok_or_else(|| missing(id))?
            .queue
            .lock()
            .unwrap()
            .drain();
        result
    }

    pub(crate) async fn remove_watcher(&self, id: &str) -> Result<(), DomainError> {
        #[cfg(test)]
        let capture_tag = format!("capture-{id}");
        #[cfg(test)]
        let _attempt =
            crate::filesystem::snapshot_test::attempt_at("WatcherRemove", Some(&capture_tag));
        let mut done = {
            let entries = self.polling.entries.lock().unwrap();
            let watcher = entries.get(id).ok_or_else(|| missing(id))?;
            watcher.cancel.0.store(true, Ordering::Release);
            watcher.done.clone()
        };
        let registry = self.polling.clone();
        let id = id.to_owned();
        // Close completes even if the caller disconnects. Keep the ID visible
        // until the producer has closed its descriptors, as upstream does.
        tokio::spawn(async move {
            while !*done.borrow_and_update() {
                done.changed().await.map_err(|_| DomainError::Internal)?;
            }
            if let Some(watcher) = registry.entries.lock().unwrap().remove(&id) {
                let mut queue = watcher.queue.lock().unwrap();
                queue.removed = true;
                queue.events.clear();
            }
            #[cfg(test)]
            crate::filesystem::snapshot_test::barrier("WatcherRemoved", Some(&capture_tag)).await?;
            Ok(())
        })
        .await
        .map_err(|_| DomainError::Internal)?
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};

    async fn server() -> (
        u16,
        Arc<crate::transport::rest::AppState>,
        tokio::task::JoinHandle<()>,
    ) {
        let lifecycle = Arc::new(LifecycleState::new());
        assert!(lifecycle.try_transition(ServerPhase::Listening));
        assert!(lifecycle.try_transition(ServerPhase::Ready));
        let state = crate::transport::new_app_state(lifecycle);
        let router = crate::transport::build_router(state.clone());
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        (
            port,
            state,
            tokio::spawn(async move {
                axum::serve(listener, router).await.unwrap();
            }),
        )
    }

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
    async fn health_rejects_stopped_watcher_reaper_without_draining_events() {
        let dir = tempfile::tempdir().unwrap();
        let (port, state, server) = server().await;
        let watcher = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
        assert!(watcher["watcherId"].is_string());
        let id = watcher["watcherId"].as_str().unwrap();
        std::fs::create_dir(dir.path().join("retained")).unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            while state.filesystem.watcher_snapshot_observation()["watchers"][id]["events"]
                .as_array()
                .unwrap()
                .is_empty()
            {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        let before =
            state.filesystem.watcher_snapshot_observation()["watchers"][id]["events"].clone();
        let reaper = state
            .filesystem
            .polling
            .reaper
            .lock()
            .unwrap()
            .clone()
            .unwrap();
        reaper.abort();
        tokio::time::timeout(Duration::from_secs(5), async {
            while !reaper.is_finished() {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        let status = reqwest::get(format!("http://127.0.0.1:{port}/health"))
            .await
            .unwrap()
            .status();
        assert_eq!(
            state.filesystem.watcher_snapshot_observation()["watchers"][id]["events"],
            before
        );
        server.abort();
        state.filesystem.polling.entries.lock().unwrap().clear();
        assert_eq!(status, http::StatusCode::SERVICE_UNAVAILABLE);
    }

    #[tokio::test]
    async fn health_rejects_corrupt_watcher_registry_without_a_business_request() {
        let (port, state, server) = server().await;
        let registry = state.filesystem.polling.clone();
        // Corrupt the actual manager lock; no watcher request runs its reaper
        // or triggers request-panic supervision before the public health probe.
        assert!(std::thread::spawn(move || {
            let _entries = registry.entries.lock().unwrap();
            panic!("injected watcher registry corruption");
        })
        .join()
        .is_err());
        let status = reqwest::get(format!("http://127.0.0.1:{port}/health"))
            .await
            .unwrap()
            .status();
        server.abort();
        assert_eq!(status, http::StatusCode::SERVICE_UNAVAILABLE);
    }

    #[tokio::test]
    async fn observed_accepted_queue_is_not_drained_by_fresh_clients_or_other_watchers() {
        let dir = tempfile::tempdir().unwrap();
        let (port, state, server) = server().await;
        let first = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
        let id = first["watcherId"].as_str().unwrap();
        std::fs::create_dir(dir.path().join("captured")).unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            while state.filesystem.watcher_snapshot_observation()["watchers"][id]["events"]
                .as_array()
                .unwrap()
                .is_empty()
            {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        let second = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
        let second = json!({"watcherId":second["watcherId"]});
        let empty = rpc(port, "GetWatcherEvents", second.clone()).await;
        assert!(empty.get("code").is_none());
        assert!(empty["events"].as_array().is_none_or(Vec::is_empty));
        assert_eq!(rpc(port, "RemoveWatcher", second).await, json!({}));
        let query = json!({"watcherId":id});
        assert_eq!(
            rpc(port, "GetWatcherEvents", query.clone()).await["events"],
            json!([{"name":"captured","type":"EVENT_TYPE_CREATE"}])
        );
        assert!(
            state.filesystem.watcher_snapshot_observation()["watchers"][id]["events"]
                .as_array()
                .unwrap()
                .is_empty()
        );
        assert_eq!(rpc(port, "RemoveWatcher", query).await, json!({}));
        assert_eq!(
            state.filesystem.watcher_snapshot_observation()["watchers"],
            json!({})
        );
        server.abort();
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "long-running HTTP observation fixture, explicitly launched in a snapshot test VM"]
    async fn snapshot_server() {
        crate::filesystem::snapshot_test::enable();
        eprintln!("watcher observation fixture revision={}", crate::REVISION);
        crate::server::run_server(crate::server::ServerConfig {
            port: 49983,
            is_not_fc: true,
        })
        .await
        .unwrap();
    }

    #[tokio::test]
    async fn public_create_retries_injected_id_collision_without_replacing_existing_watcher() {
        let dir = tempfile::tempdir().unwrap();
        let (port, state, server) = server().await;
        state
            .filesystem
            .polling
            .id_candidates
            .lock()
            .unwrap()
            .extend(["wcollision".into(), "wcollision".into(), "wretry".into()]);
        let first = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
        let second = rpc(port, "CreateWatcher", json!({"path":dir.path()})).await;
        assert_eq!(first["watcherId"], "wcollision");
        assert_eq!(second["watcherId"], "wretry");
        for created in [first, second] {
            let query = json!({"watcherId":created["watcherId"]});
            assert!(rpc(port, "GetWatcherEvents", query.clone())
                .await
                .get("code")
                .is_none());
            assert_eq!(rpc(port, "RemoveWatcher", query).await, json!({}));
        }
        server.abort();
    }

    #[tokio::test]
    async fn removed_root_keeps_its_id_until_explicit_removal() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("watched");
        std::fs::create_dir(&root).unwrap();
        let (port, _state, server) = server().await;
        let created = rpc(port, "CreateWatcher", json!({"path":root})).await;
        let query = json!({"watcherId":created["watcherId"]});
        std::fs::remove_dir(&root).unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let response = rpc(port, "GetWatcherEvents", query.clone()).await;
                assert!(response.get("code").is_none(), "{response}");
                if response["events"].as_array().is_some_and(|events| {
                    events
                        .iter()
                        .any(|event| event["name"] == "." && event["type"] == "EVENT_TYPE_REMOVE")
                }) {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        assert_eq!(
            rpc(port, "GetWatcherEvents", query.clone()).await,
            json!({})
        );
        assert_eq!(rpc(port, "RemoveWatcher", query.clone()).await, json!({}));
        assert_eq!(
            rpc(port, "GetWatcherEvents", query).await["code"],
            "not_found"
        );
        server.abort();
    }
    fn inode_is_watched(inode: u64) -> bool {
        std::fs::read_dir("/proc/self/fdinfo")
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

    async fn cleaned(inode: u64) {
        tokio::time::timeout(Duration::from_secs(5), async {
            while inode_is_watched(inode) {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("watch resources released");
    }

    #[tokio::test]
    async fn captured_get_response_survives_remove_while_new_get_is_not_found() {
        use std::os::unix::fs::MetadataExt;
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("root");
        std::fs::create_dir(&root).unwrap();
        let inode = root.metadata().unwrap().ino();
        let (port, state, server) = server().await;
        let created = rpc(port, "CreateWatcher", json!({"path":root})).await;
        let query = json!({"watcherId":created["watcherId"]});
        std::fs::create_dir(root.join("prefix")).unwrap();
        std::fs::remove_dir(root.join("prefix")).unwrap();
        std::fs::remove_dir(&root).unwrap();
        // Kernel watch removal can precede the worker reading queued DELETE_SELF.
        // Observe the existing queue without draining it before capturing HTTP headers.
        let id = created["watcherId"].as_str().unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            while state.filesystem.watcher_snapshot_observation()["watchers"][id]["events"]
                .as_array()
                .unwrap()
                .len()
                != 3
            {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        cleaned(inode).await;
        // Receiving headers fences Get's queue exchange, but leave its body unread
        // until Remove has completed on another HTTP connection.
        let captured = reqwest::Client::new()
            .post(format!(
                "http://127.0.0.1:{port}/filesystem.Filesystem/GetWatcherEvents"
            ))
            .header("connect-protocol-version", "1")
            .json(&query)
            .send()
            .await
            .unwrap();
        assert_eq!(captured.status(), 200);
        assert_eq!(rpc(port, "RemoveWatcher", query.clone()).await, json!({}));
        assert_eq!(
            rpc(port, "GetWatcherEvents", query).await["code"],
            "not_found"
        );
        assert_eq!(
            captured.json::<Value>().await.unwrap()["events"],
            json!([
                {"name":"prefix","type":"EVENT_TYPE_CREATE"}, {"name":"prefix","type":"EVENT_TYPE_REMOVE"}, {"name":".","type":"EVENT_TYPE_REMOVE"}
            ])
        );
        server.abort();
    }
}

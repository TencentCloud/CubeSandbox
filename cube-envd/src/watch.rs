use crate::{
    connect::{decode_frame, encode_end_stream, encode_stream_message, ConnectError},
    filesystem,
};
use axum::{
    body::{Body, Bytes},
    extract::Json,
    http::{header, HeaderMap, StatusCode},
    response::Response,
};
use inotify::{EventMask, Inotify, WatchDescriptor, WatchMask};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashMap,
    io,
    os::fd::AsRawFd,
    path::{Path, PathBuf},
    pin::Pin,
    sync::{
        atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
        Arc, Mutex, OnceLock,
    },
    task::{Context, Poll},
    time::Duration,
};
use tokio::sync::{mpsc, oneshot};
use tokio_stream::Stream;

static ACTIVE_WATCHERS: AtomicUsize = AtomicUsize::new(0);

/// Cap on buffered pull-watcher events. A client that never calls
/// `GetWatcherEvents` must not be able to grow the daemon's memory without
/// bound; the oldest events are dropped once the cap is reached.
const MAX_PULL_EVENTS: usize = 10_000;

#[derive(Debug, Deserialize)]
struct WatchDirRequest {
    path: String,
}

#[derive(Debug, Serialize)]
struct WatchResponse {
    #[serde(skip_serializing_if = "Option::is_none")]
    start: Option<EmptyEvent>,
    #[serde(skip_serializing_if = "Option::is_none")]
    filesystem: Option<WatchEvent>,
    #[serde(skip_serializing_if = "Option::is_none")]
    keepalive: Option<EmptyEvent>,
}

#[derive(Debug, Serialize)]
struct WatchEvent {
    name: String,
    #[serde(rename = "type")]
    event_type: String,
}

#[derive(Debug, Serialize)]
struct EmptyEvent {}

pub async fn watch_dir(headers: HeaderMap, body: Bytes) -> Response {
    let request = match decode_request(&body) {
        Ok(request) => request,
        Err(error) => return filesystem_error(StatusCode::BAD_REQUEST, error),
    };
    let username = match filesystem::request_user(&headers) {
        Ok(username) => username,
        Err(error) => return filesystem::fs_error_response(error),
    };
    let path = match filesystem::existing_path(&request.path) {
        Ok(path) => path,
        Err(error) => return filesystem::fs_error_response(error),
    };
    if let Err(error) = filesystem::validate_directory(&path, &username) {
        return filesystem::fs_error_response(error);
    }

    let inotify = match Inotify::init() {
        Ok(inotify) => inotify,
        Err(error) => {
            return filesystem_error(StatusCode::INTERNAL_SERVER_ERROR, error.to_string())
        }
    };
    if let Err(error) = set_nonblocking(&inotify) {
        return filesystem_error(StatusCode::INTERNAL_SERVER_ERROR, error.to_string());
    }
    let watch_descriptor = match inotify.watches().add(
        &path,
        WatchMask::CREATE
            | WatchMask::DELETE
            | WatchMask::MODIFY
            | WatchMask::ATTRIB
            | WatchMask::MOVED_FROM
            | WatchMask::MOVED_TO,
    ) {
        Ok(descriptor) => descriptor,
        Err(error) => {
            return filesystem_error(StatusCode::INTERNAL_SERVER_ERROR, error.to_string())
        }
    };

    let (sender, receiver) = mpsc::channel::<Result<Bytes, io::Error>>(32);
    let _ = send_frame(
        &sender,
        WatchResponse {
            start: Some(EmptyEvent {}),
            filesystem: None,
            keepalive: None,
        },
    )
    .await;
    let (cancel_sender, cancel_receiver) = oneshot::channel();
    ACTIVE_WATCHERS.fetch_add(1, Ordering::SeqCst);
    tokio::spawn(run_watcher(
        inotify,
        watch_descriptor,
        sender,
        cancel_receiver,
    ));

    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/connect+json")
        .body(Body::from_stream(WatchResponseStream {
            receiver,
            cancel: Some(cancel_sender),
        }))
        .expect("valid WatchDir stream response")
}

static WATCHERS: OnceLock<Mutex<HashMap<String, Arc<PullWatcher>>>> = OnceLock::new();
static WATCHER_SEQUENCE: AtomicU64 = AtomicU64::new(1);

fn watchers() -> &'static Mutex<HashMap<String, Arc<PullWatcher>>> {
    WATCHERS.get_or_init(|| Mutex::new(HashMap::new()))
}

struct PullWatcher {
    events: Mutex<Vec<WatchEvent>>,
    stop: AtomicBool,
}

#[derive(Debug, Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct CreateWatcherRequest {
    path: String,
    #[serde(default)]
    recursive: bool,
    #[serde(default)]
    include_entry: bool,
    #[serde(default)]
    allow_network_mounts: bool,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct CreateWatcherResponse {
    watcher_id: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct GetWatcherEventsRequest {
    watcher_id: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct GetWatcherEventsResponse {
    events: Vec<WatchEvent>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct RemoveWatcherRequest {
    watcher_id: String,
}

/// Non-streaming watcher: buffers inotify events so a caller can pull them.
pub async fn create_watcher(
    headers: HeaderMap,
    Json(request): Json<CreateWatcherRequest>,
) -> Response {
    let username = match filesystem::request_user(&headers) {
        Ok(username) => username,
        Err(error) => return filesystem::fs_error_response(error),
    };
    let path = match filesystem::existing_path(&request.path) {
        Ok(path) => path,
        Err(error) => return filesystem::fs_error_response(error),
    };
    if let Err(error) = filesystem::validate_directory(&path, &username) {
        return filesystem::fs_error_response(error);
    }
    let inotify = match Inotify::init() {
        Ok(inotify) => inotify,
        Err(error) => {
            return filesystem_error(StatusCode::INTERNAL_SERVER_ERROR, error.to_string())
        }
    };
    if let Err(error) = set_nonblocking(&inotify) {
        return filesystem_error(StatusCode::INTERNAL_SERVER_ERROR, error.to_string());
    }
    if let Err(error) = add_watches(&inotify, &path, request.recursive) {
        return filesystem_error(StatusCode::INTERNAL_SERVER_ERROR, error.to_string());
    }
    let watcher_id = format!(
        "w-{}-{}",
        std::process::id(),
        WATCHER_SEQUENCE.fetch_add(1, Ordering::Relaxed)
    );
    let watcher = Arc::new(PullWatcher {
        events: Mutex::new(Vec::new()),
        stop: AtomicBool::new(false),
    });
    watchers()
        .lock()
        .expect("watcher registry lock poisoned")
        .insert(watcher_id.clone(), Arc::clone(&watcher));
    let _ = (request.include_entry, request.allow_network_mounts);
    tokio::task::spawn_blocking(move || run_pull_watcher(inotify, watcher));
    json_response(StatusCode::OK, CreateWatcherResponse { watcher_id })
}

pub async fn get_watcher_events(Json(request): Json<GetWatcherEventsRequest>) -> Response {
    let watcher = match watchers()
        .lock()
        .expect("watcher registry lock poisoned")
        .get(&request.watcher_id)
        .cloned()
    {
        Some(watcher) => watcher,
        None => return watcher_not_found(),
    };
    let events = std::mem::take(&mut *watcher.events.lock().expect("watcher events lock poisoned"));
    json_response(StatusCode::OK, GetWatcherEventsResponse { events })
}

pub async fn remove_watcher(Json(request): Json<RemoveWatcherRequest>) -> Response {
    if let Some(watcher) = watchers()
        .lock()
        .expect("watcher registry lock poisoned")
        .remove(&request.watcher_id)
    {
        watcher.stop.store(true, Ordering::Release);
    }
    json_response(StatusCode::OK, serde_json::json!({}))
}

fn run_pull_watcher(mut inotify: Inotify, watcher: Arc<PullWatcher>) {
    let mut buffer = vec![0u8; 16 * 1024];
    while !watcher.stop.load(Ordering::Acquire) {
        std::thread::sleep(Duration::from_millis(50));
        match inotify.read_events(&mut buffer) {
            Ok(events) => {
                let collected: Vec<WatchEvent> = events
                    .filter_map(|event| {
                        let name = event.name?.to_string_lossy().into_owned();
                        let event_type = event_type(event.mask)?;
                        Some(WatchEvent { name, event_type })
                    })
                    .collect();
                if !collected.is_empty() {
                    let mut buffer = watcher.events.lock().expect("watcher events lock poisoned");
                    push_bounded(&mut buffer, collected);
                }
            }
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => {}
            Err(_) => break,
        }
    }
}

/// Append events, keeping at most [`MAX_PULL_EVENTS`] (dropping the oldest).
fn push_bounded(buffer: &mut Vec<WatchEvent>, mut new_events: Vec<WatchEvent>) {
    buffer.append(&mut new_events);
    if buffer.len() > MAX_PULL_EVENTS {
        let overflow = buffer.len() - MAX_PULL_EVENTS;
        buffer.drain(0..overflow);
    }
}

fn add_watches(inotify: &Inotify, path: &Path, recursive: bool) -> io::Result<()> {
    let mask = WatchMask::CREATE
        | WatchMask::DELETE
        | WatchMask::MODIFY
        | WatchMask::ATTRIB
        | WatchMask::MOVED_FROM
        | WatchMask::MOVED_TO;
    inotify
        .watches()
        .add(path, mask)
        .map_err(|error| io::Error::other(error.to_string()))?;
    if recursive {
        let mut budget = 1024usize;
        let mut stack: Vec<PathBuf> = vec![path.to_path_buf()];
        while let Some(directory) = stack.pop() {
            if budget == 0 {
                break;
            }
            if let Ok(entries) = std::fs::read_dir(&directory) {
                for entry in entries.flatten() {
                    if entry.file_type().map(|kind| kind.is_dir()).unwrap_or(false) {
                        let child = entry.path();
                        if inotify.watches().add(&child, mask).is_ok() {
                            budget -= 1;
                        }
                        stack.push(child);
                    }
                }
            }
        }
    }
    Ok(())
}

fn watcher_not_found() -> Response {
    filesystem::fs_error_response(io::Error::new(io::ErrorKind::NotFound, "watcher not found"))
}

fn json_response<T: Serialize>(status: StatusCode, value: T) -> Response {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            serde_json::to_vec(&value).expect("watch response serializes"),
        ))
        .expect("valid watch response")
}

async fn run_watcher(
    mut inotify: Inotify,
    watch_descriptor: WatchDescriptor,
    sender: mpsc::Sender<Result<Bytes, io::Error>>,
    mut cancel: oneshot::Receiver<()>,
) {
    let mut buffer = vec![0u8; 16 * 1024];
    let mut last_event = tokio::time::Instant::now();
    loop {
        tokio::select! {
            _ = &mut cancel => break,
            _ = tokio::time::sleep(Duration::from_millis(50)) => {}
        }

        let changes = match inotify.read_events(&mut buffer) {
            Ok(events) => events
                .filter_map(|event| {
                    let name = event.name?.to_string_lossy().into_owned();
                    let event_type = event_type(event.mask)?;
                    Some((name, event_type))
                })
                .collect::<Vec<_>>(),
            Err(error) if error.kind() == io::ErrorKind::WouldBlock => Vec::new(),
            Err(error) => {
                send_end_error(&sender, error.to_string()).await;
                break;
            }
        };

        for (name, event_type) in changes {
            last_event = tokio::time::Instant::now();
            if send_frame(
                &sender,
                WatchResponse {
                    start: None,
                    filesystem: Some(WatchEvent { name, event_type }),
                    keepalive: None,
                },
            )
            .await
            .is_err()
            {
                cleanup_watcher(&mut inotify, watch_descriptor);
                ACTIVE_WATCHERS.fetch_sub(1, Ordering::SeqCst);
                return;
            }
        }

        if tokio::time::Instant::now().duration_since(last_event) >= Duration::from_secs(30) {
            if send_frame(
                &sender,
                WatchResponse {
                    start: None,
                    filesystem: None,
                    keepalive: Some(EmptyEvent {}),
                },
            )
            .await
            .is_err()
            {
                break;
            }
            last_event = tokio::time::Instant::now();
        }
    }
    cleanup_watcher(&mut inotify, watch_descriptor);
    ACTIVE_WATCHERS.fetch_sub(1, Ordering::SeqCst);
}

fn cleanup_watcher(inotify: &mut Inotify, watch_descriptor: WatchDescriptor) {
    let _ = inotify.watches().remove(watch_descriptor);
}

fn set_nonblocking(inotify: &Inotify) -> io::Result<()> {
    use nix::fcntl::{fcntl, FcntlArg, OFlag};

    let flags = fcntl(inotify.as_raw_fd(), FcntlArg::F_GETFL)
        .map_err(|error| io::Error::other(error.to_string()))?;
    let flags = OFlag::from_bits_truncate(flags) | OFlag::O_NONBLOCK;
    fcntl(inotify.as_raw_fd(), FcntlArg::F_SETFL(flags))
        .map(|_| ())
        .map_err(|error| io::Error::other(error.to_string()))
}

fn event_type(mask: EventMask) -> Option<String> {
    if mask.contains(EventMask::CREATE) {
        Some("EVENT_TYPE_CREATE".to_owned())
    } else if mask.contains(EventMask::DELETE) {
        Some("EVENT_TYPE_REMOVE".to_owned())
    } else if mask.contains(EventMask::MODIFY) {
        Some("EVENT_TYPE_WRITE".to_owned())
    } else if mask.intersects(EventMask::MOVED_FROM | EventMask::MOVED_TO) {
        Some("EVENT_TYPE_RENAME".to_owned())
    } else if mask.contains(EventMask::ATTRIB) {
        Some("EVENT_TYPE_CHMOD".to_owned())
    } else {
        None
    }
}

fn decode_request(body: &[u8]) -> Result<WatchDirRequest, String> {
    let (flags, payload) = decode_frame(body).map_err(|error| error.to_string())?;
    if flags != 0 {
        return Err("WatchDir request must be a regular Connect frame".to_owned());
    }
    serde_json::from_slice(&payload).map_err(|error| error.to_string())
}

async fn send_frame<T: Serialize>(
    sender: &mpsc::Sender<Result<Bytes, io::Error>>,
    value: T,
) -> Result<(), mpsc::error::SendError<Result<Bytes, io::Error>>> {
    let payload = serde_json::to_vec(&value).expect("WatchDir response serializes");
    sender
        .send(Ok(Bytes::from(encode_stream_message(&payload))))
        .await
}

async fn send_end_error(sender: &mpsc::Sender<Result<Bytes, io::Error>>, message: String) {
    let error = ConnectError::new("internal", message);
    let _ = sender
        .send(Ok(Bytes::from(encode_end_stream(Some(error)))))
        .await;
}

struct WatchResponseStream {
    receiver: mpsc::Receiver<Result<Bytes, io::Error>>,
    cancel: Option<oneshot::Sender<()>>,
}

impl Stream for WatchResponseStream {
    type Item = Result<Bytes, io::Error>;

    fn poll_next(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        self.receiver.poll_recv(context)
    }
}

impl Drop for WatchResponseStream {
    fn drop(&mut self) {
        if let Some(cancel) = self.cancel.take() {
            let _ = cancel.send(());
        }
    }
}

fn filesystem_error(status: StatusCode, message: String) -> Response {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(
            serde_json::json!({"code": "invalid_argument", "message": message}).to_string(),
        ))
        .expect("valid WatchDir error response")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use tempfile::tempdir;
    use tokio::time::{sleep, timeout};
    use tokio_stream::StreamExt;

    async fn next_json(
        stream: &mut (impl Stream<Item = Result<Bytes, axum::Error>> + Unpin),
    ) -> serde_json::Value {
        let chunk = timeout(Duration::from_secs(2), stream.next())
            .await
            .expect("WatchDir event timed out")
            .expect("WatchDir stream ended")
            .expect("WatchDir stream failed");
        let (flags, payload) = decode_frame(&chunk).expect("valid Connect frame");
        assert_eq!(flags, 0);
        serde_json::from_slice(&payload).expect("valid WatchDir JSON")
    }

    fn request(path: &std::path::Path) -> Bytes {
        let payload = serde_json::json!({"path": path.to_string_lossy()});
        Bytes::from(encode_stream_message(payload.to_string().as_bytes()))
    }

    #[test]
    fn event_types_match_upstream_filesystem_protocol() {
        assert_eq!(
            event_type(EventMask::CREATE).as_deref(),
            Some("EVENT_TYPE_CREATE")
        );
        assert_eq!(
            event_type(EventMask::DELETE).as_deref(),
            Some("EVENT_TYPE_REMOVE")
        );
        assert_eq!(
            event_type(EventMask::MODIFY).as_deref(),
            Some("EVENT_TYPE_WRITE")
        );
        assert_eq!(
            event_type(EventMask::MOVED_FROM).as_deref(),
            Some("EVENT_TYPE_RENAME")
        );
        assert_eq!(
            event_type(EventMask::ATTRIB).as_deref(),
            Some("EVENT_TYPE_CHMOD")
        );
    }

    #[cfg_attr(not(target_os = "linux"), ignore)]
    #[tokio::test]
    async fn watch_dir_reports_create_remove_and_releases_watcher() {
        let directory = tempdir().unwrap();
        let response = watch_dir(HeaderMap::new(), request(directory.path())).await;
        assert_eq!(response.status(), StatusCode::OK);
        let mut stream = response.into_body().into_data_stream();
        let start = next_json(&mut stream).await;
        assert!(start["start"].is_object());

        let active_before = ACTIVE_WATCHERS.load(Ordering::SeqCst);
        let file = directory.path().join("a.txt");
        fs::write(&file, b"hello").unwrap();
        loop {
            let event = next_json(&mut stream).await;
            if event["filesystem"]["name"] == "a.txt" {
                break;
            }
        }
        fs::remove_file(&file).unwrap();
        loop {
            let event = next_json(&mut stream).await;
            if event["filesystem"]["name"] == "a.txt"
                && event["filesystem"]["type"] == "EVENT_TYPE_REMOVE"
            {
                break;
            }
        }

        drop(stream);
        timeout(Duration::from_secs(2), async {
            loop {
                if ACTIVE_WATCHERS.load(Ordering::SeqCst) <= active_before {
                    break;
                }
                sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("WatchDir watcher was not released");
    }

    #[test]
    fn unknown_event_mask_has_no_type() {
        assert!(event_type(EventMask::empty()).is_none());
    }

    #[tokio::test]
    async fn watch_dir_missing_path_returns_not_found() {
        let payload = encode_stream_message(
            serde_json::json!({"path": "/tmp/cube-envd-no-such-dir"})
                .to_string()
                .as_bytes(),
        );
        let response = watch_dir(HeaderMap::new(), Bytes::from(payload)).await;
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn pull_watcher_reports_events_and_can_be_removed() {
        use axum::body::to_bytes;
        use std::fs;
        use tempfile::tempdir;

        let directory = tempdir().unwrap();
        let response = create_watcher(
            HeaderMap::new(),
            Json(CreateWatcherRequest {
                path: directory.path().to_string_lossy().into_owned(),
                recursive: false,
                include_entry: false,
                allow_network_mounts: false,
            }),
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        let body = to_bytes(response.into_body(), 64 * 1024).await.unwrap();
        let watcher_id = serde_json::from_slice::<serde_json::Value>(&body).unwrap()["watcherId"]
            .as_str()
            .unwrap()
            .to_owned();

        fs::write(directory.path().join("created.txt"), b"x").unwrap();

        let mut saw = false;
        for _ in 0..40 {
            let response = get_watcher_events(Json(GetWatcherEventsRequest {
                watcher_id: watcher_id.clone(),
            }))
            .await;
            let status = response.status();
            let body = to_bytes(response.into_body(), 64 * 1024).await.unwrap();
            let events = serde_json::from_slice::<serde_json::Value>(&body).unwrap();
            if status == StatusCode::OK
                && events["events"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|event| event["name"] == "created.txt")
            {
                saw = true;
                break;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        assert!(saw, "pull watcher did not report the created file");

        let response = remove_watcher(Json(RemoveWatcherRequest { watcher_id })).await;
        assert_eq!(response.status(), StatusCode::OK);

        let response = get_watcher_events(Json(GetWatcherEventsRequest {
            watcher_id: "w-missing".to_owned(),
        }))
        .await;
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[test]
    fn pull_watcher_buffer_is_bounded() {
        let events = |prefix: &str, count: usize| -> Vec<WatchEvent> {
            (0..count)
                .map(|index| WatchEvent {
                    name: format!("{prefix}{index}"),
                    event_type: "EVENT_TYPE_CREATE".to_owned(),
                })
                .collect()
        };

        let mut buffer = Vec::new();
        push_bounded(&mut buffer, events("a", MAX_PULL_EVENTS));
        assert_eq!(buffer.len(), MAX_PULL_EVENTS);

        push_bounded(&mut buffer, events("b", 5));
        assert_eq!(buffer.len(), MAX_PULL_EVENTS);
        assert_eq!(buffer.last().unwrap().name, "b4");
    }
}

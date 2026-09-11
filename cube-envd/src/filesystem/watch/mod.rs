// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use super::*;
use crate::proto::filesystem::{
    watch_dir_response, EventType, FilesystemEvent, WatchDirRequest, WatchDirResponse,
};
use crate::server::{LifecycleState, ServerPhase};
use std::collections::{HashMap, HashSet, VecDeque};
use std::ffi::OsString;
use std::time::{Duration, Instant};
use tokio::sync::mpsc;

pub(super) mod polling;

const QUEUE: usize = 1;
const MASK: u32 = libc::IN_CREATE
    | libc::IN_MODIFY
    | libc::IN_DELETE
    | libc::IN_ATTRIB
    | libc::IN_MOVED_FROM
    | libc::IN_MOVED_TO
    | libc::IN_DELETE_SELF
    | libc::IN_MOVE_SELF
    | libc::IN_ONLYDIR;

pub(crate) struct Subscription {
    events: mpsc::Receiver<WatchDirResponse>,
    terminal: oneshot::Receiver<DomainError>,
    _cancel: CancelOnDrop,
}

impl Subscription {
    pub fn stream(
        self,
        interval: std::time::Duration,
    ) -> connectrpc::ServiceStream<WatchDirResponse> {
        use futures::StreamExt;
        let start = WatchDirResponse {
            event: Some(watch_dir_response::Event::Start(Box::default())),
            ..Default::default()
        };
        let events = futures::stream::unfold(Some(self), move |state| async move {
            let mut subscription = state?;
            let event = tokio::select! {
                biased;
                event = subscription.events.recv() => event,
                _ = tokio::time::sleep(interval) => Some(WatchDirResponse { event: Some(watch_dir_response::Event::Keepalive(Box::default())), ..Default::default() }),
            };
            if let Some(event) = event {
                return Some((Ok(event), Some(subscription)));
            }
            let error = subscription.terminal.await.unwrap_or(DomainError::Internal);
            Some((
                Err(connectrpc::ConnectError::new(
                    error.connect_code(),
                    error.public_message(),
                )),
                None,
            ))
        });
        Box::pin(futures::stream::once(async { Ok(start) }).chain(events))
    }
}

impl FilesystemService {
    pub(crate) async fn watch_dir(
        &self,
        request: WatchDirRequest,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
        lifecycle: Arc<LifecycleState>,
    ) -> Result<Subscription, DomainError> {
        let (send, events) = mpsc::channel(QUEUE);
        let send = Sink::Streaming(send, lifecycle.clone());
        let mut backend = self
            .prepare_watch(request, headers, snapshot, users)
            .await?;
        let (terminal, receive) = oneshot::channel();
        let cancel = CancelOnDrop(Arc::new(AtomicBool::new(false)));
        let stopped = cancel.0.clone();
        std::thread::Builder::new()
            .name("envd-watch".into())
            .spawn(move || {
                let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    backend.run(&send, &stopped, &lifecycle)
                }));
                let error = result.unwrap_or_else(|_| {
                    lifecycle.fail("watch worker panic");
                    DomainError::Internal
                });
                // Drop backend resources before making the terminal visible; the
                // bounded accepted prefix remains owned by this subscription.
                drop(backend);
                let _ = terminal.send(error);
            })
            .map_err(|_| exhausted())?;
        Ok(Subscription {
            events,
            terminal: receive,
            _cancel: cancel,
        })
    }

    async fn prepare_watch(
        &self,
        request: WatchDirRequest,
        headers: &http::HeaderMap,
        snapshot: Arc<RuntimeState>,
        users: UserDatabase,
    ) -> Result<Backend, DomainError> {
        self.run(headers, snapshot, users, move |context| {
            let path = context.rpc_path(&request.path)?;
            let failure = |error: std::io::Error| {
                let detail = format!("stat {}: {}", path.display(), io_detail(&error));
                if error.raw_os_error() == Some(libc::ENOENT) {
                    DomainError::NotFound(format!("path {} not found: {detail}", path.display()))
                } else {
                    DomainError::InternalMessage(format!(
                        "error statting path {}: {detail}",
                        path.display()
                    ))
                }
            };
            validate_os_path(&path).map_err(failure)?;
            let (parent, name) = context.parent_with_error(&path, failure)?;
            context.check()?;
            let root = open_at(&parent, &name, 0).map_err(failure)?;
            if !root.metadata().map_err(lookup_error)?.is_dir() {
                return Err(invalid(&format!(
                    "path {} not a directory: %!w(<nil>)",
                    path.display()
                )));
            }
            supported_backend(&root).map_err(|error| {
                if error == invalid("unsupported watch filesystem") {
                    invalid(&format!(
                        "cannot watch path on network filesystem: {}",
                        path.display()
                    ))
                } else {
                    DomainError::InternalMessage(format!(
                        "error checking mount status: {}",
                        error.public_message()
                    ))
                }
            })?;
            let fd = owned_fd(unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) })
                .map_err(|error| {
                    DomainError::InternalMessage(format!(
                        "error creating watcher: {}",
                        io_detail(&error)
                    ))
                })?;
            if request.recursive {
                let clean: PathBuf = path.components().collect();
                let (clean_parent, clean_name) = context.parent(&clean)?;
                let entry =
                    open_at(&clean_parent, &clean_name, libc::O_NOFOLLOW).map_err(watch_error)?;
                if entry.metadata().map_err(watch_error)?.is_symlink() {
                    return Err(DomainError::InternalMessage(format!(
                        "error adding path {} to watcher: fsnotify: not a directory: {:?}",
                        path.display(),
                        clean
                    )));
                }
            }
            let mut backend = Backend {
                fd,
                requested_path: path.clone(),
                root_path: std::fs::read_link(format!("/proc/self/fd/{}", root.as_raw_fd()))
                    .map_err(watch_error)?,
                dirs: HashMap::new(),
                recursive: request.recursive,
                pending: VecDeque::new(),
                moves: HashMap::new(),
                retired: HashSet::new(),
                waiting: None,
            };
            backend
                .install(root, PathBuf::new(), &context.cancel, None)
                .map_err(|error| match error {
                    DomainError::Cancelled => error,
                    _ => DomainError::InternalMessage(format!(
                        "error adding path {} to watcher: {}",
                        path.display(),
                        error.public_message()
                    )),
                })?;
            context.check()?;
            Ok(backend)
        })
        .await
    }
}

#[derive(Clone)]
enum Sink {
    Streaming(mpsc::Sender<WatchDirResponse>, Arc<LifecycleState>),
    Polling(Arc<std::sync::Mutex<polling::Queue>>),
}

impl Sink {
    fn is_closed(&self) -> bool {
        match self {
            Self::Streaming(send, _) => send.is_closed(),
            Self::Polling(queue) => queue.lock().unwrap().removed,
        }
    }

    fn append(&self, event: FilesystemEvent, cancel: &AtomicBool) -> Result<(), DomainError> {
        match self {
            Self::Streaming(send, lifecycle) => {
                let mut response = WatchDirResponse {
                    event: Some(watch_dir_response::Event::Filesystem(Box::new(event))),
                    ..Default::default()
                };
                loop {
                    check_cancel(cancel)?;
                    if matches!(
                        lifecycle.phase(),
                        ServerPhase::Draining | ServerPhase::Stopped | ServerPhase::Failed
                    ) {
                        return Err(DomainError::Cancelled);
                    }
                    match send.try_send(response) {
                        Ok(()) => return Ok(()),
                        Err(mpsc::error::TrySendError::Closed(_)) => {
                            return Err(DomainError::Cancelled)
                        }
                        Err(mpsc::error::TrySendError::Full(pending)) => response = pending,
                    }
                    // This dedicated inotify thread waits for transport delivery;
                    // request cancellation and daemon shutdown still unblock it.
                    std::thread::sleep(Duration::from_millis(5));
                }
            }
            Self::Polling(queue) => queue.lock().unwrap().append(event),
        }
    }
}

struct Directory {
    dev: u64,
    ino: u64,
    path: PathBuf,
}
impl Directory {
    fn event_path(&self, name: &OsStr) -> PathBuf {
        if name.is_empty() {
            self.path.clone()
        } else {
            self.path.join(name)
        }
    }
}
struct Backend {
    fd: File,
    requested_path: PathBuf,
    root_path: PathBuf,
    dirs: HashMap<i32, Directory>,
    recursive: bool,
    pending: VecDeque<RawEvent>,
    moves: HashMap<u32, PathBuf>,
    retired: HashSet<i32>,
    waiting: Option<Instant>,
}
struct RawEvent {
    wd: i32,
    mask: u32,
    cookie: u32,
    name: std::ffi::OsString,
    // Events read before a move-out's removal point retain their old mapping.
    detached: Option<PathBuf>,
}
impl Backend {
    fn install(
        &mut self,
        object: File,
        path: PathBuf,
        cancel: &AtomicBool,
        synthetic: Option<&Sink>,
    ) -> Result<(), DomainError> {
        // Walk in lexical depth-first order without recursive Rust calls or
        // opening every child at once. Completed directory iterators close here.
        let mut pending = vec![(object, path, None::<std::vec::IntoIter<OsString>>)];
        while let Some((object, path, children)) = pending.pop() {
            check_cancel(cancel)?;
            if let Some(mut children) = children {
                if let Some(name) = children.next() {
                    let child = open_at(&object, &name, libc::O_NOFOLLOW).map_err(watch_error)?;
                    let child_path = path.join(name);
                    pending.push((object, path, Some(children)));
                    pending.push((child, child_path, None));
                }
                continue;
            }
            let metadata = object.metadata().map_err(watch_error)?;
            let directory = metadata.is_dir();
            let proc_path = CString::new(format!("/proc/self/fd/{}", object.as_raw_fd())).unwrap();
            if directory {
                let wd = unsafe {
                    libc::inotify_add_watch(self.fd.as_raw_fd(), proc_path.as_ptr(), MASK)
                };
                if wd < 0 {
                    return Err(watch_error(std::io::Error::last_os_error()));
                }
                self.dirs.insert(
                    wd,
                    Directory {
                        dev: metadata.dev(),
                        ino: metadata.ino(),
                        path: path.clone(),
                    },
                );
            }
            let children = if directory && self.recursive {
                let mut names = Vec::new();
                for item in std::fs::read_dir(proc_path.to_str().unwrap()).map_err(|error| {
                    DomainError::InternalMessage(format!(
                        "open {}: {}",
                        self.requested_path.join(&path).display(),
                        io_detail(&error)
                    ))
                })? {
                    check_cancel(cancel)?;
                    names.push(item.map_err(watch_error)?.file_name());
                }
                names.sort();
                Some(names.into_iter())
            } else {
                None
            };
            if let Some(send) = synthetic {
                send.append(
                    FilesystemEvent {
                        name: path.to_string_lossy().into_owned(),
                        r#type: EventType::Create.into(),
                        ..Default::default()
                    },
                    cancel,
                )?;
            }
            if let Some(children) = children {
                pending.push((object, path, Some(children)));
            }
        }
        Ok(())
    }
    fn run(&mut self, send: &Sink, cancel: &AtomicBool, lifecycle: &LifecycleState) -> DomainError {
        self.produce(send, cancel, lifecycle).unwrap_err()
    }

    fn read_events(&mut self) -> Result<(), DomainError> {
        let mut buffer = [0u8; 64 * 1024];
        loop {
            let count = unsafe {
                libc::read(
                    self.fd.as_raw_fd(),
                    buffer.as_mut_ptr().cast(),
                    buffer.len(),
                )
            };
            if count < 0 {
                let error = std::io::Error::last_os_error();
                if error.kind() == std::io::ErrorKind::WouldBlock {
                    return Ok(());
                }
                if error.kind() == std::io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(runtime_error(watch_error(error)));
            }
            if count == 0 {
                return Err(DomainError::Internal);
            }
            let mut offset = 0;
            while offset + 16 <= count as usize {
                let event = unsafe {
                    std::ptr::read_unaligned(
                        buffer[offset..].as_ptr().cast::<libc::inotify_event>(),
                    )
                };
                let end = offset + 16 + event.len as usize;
                if end > count as usize {
                    return Err(DomainError::Internal);
                }
                let name = &buffer[offset + 16..end];
                self.pending.push_back(RawEvent {
                    wd: event.wd,
                    mask: event.mask,
                    cookie: event.cookie,
                    name: OsStr::from_bytes(
                        name.split(|byte| *byte == 0).next().unwrap_or_default(),
                    )
                    .to_owned(),
                    detached: None,
                });
                offset = end;
            }
            if offset != count as usize {
                return Err(DomainError::Internal);
            }
            // Process each kernel batch before reading more, so continuous
            // producers cannot starve cancellation or transport delivery.
            return Ok(());
        }
    }

    fn produce(
        &mut self,
        send: &Sink,
        cancel: &AtomicBool,
        lifecycle: &LifecycleState,
    ) -> Result<(), DomainError> {
        loop {
            check_cancel(cancel)?;
            if send.is_closed()
                || matches!(
                    lifecycle.phase(),
                    ServerPhase::Draining | ServerPhase::Stopped | ServerPhase::Failed
                )
            {
                return Err(DomainError::Cancelled);
            }
            self.pump(send, cancel)?;
            let mut poll = libc::pollfd {
                fd: self.fd.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            };
            let timeout = if self.waiting.is_some() { 5 } else { 50 };
            if unsafe { libc::poll(&mut poll, 1, timeout) } < 0 {
                let error = std::io::Error::last_os_error();
                if error.kind() != std::io::ErrorKind::Interrupted {
                    return Err(runtime_error(watch_error(error)));
                }
            }
        }
    }

    fn pump(&mut self, send: &Sink, cancel: &AtomicBool) -> Result<(), DomainError> {
        self.read_events()?;
        while let Some(event) = self.pending.front() {
            check_cancel(cancel)?;
            // Hold the ordered pipeline at an unresolved directory move. Its
            // matching TO may be in a later kernel read. Unrelated subscribers
            // never wait on this watcher.
            if self.recursive
                && event.mask & libc::IN_ISDIR != 0
                && event.mask & libc::IN_MOVED_FROM != 0
                && event.detached.is_none()
                && !self.retired.contains(&event.wd)
            {
                let paired = self.pending.iter().skip(1).any(|other| {
                    other.mask & libc::IN_MOVED_TO != 0 && other.cookie == event.cookie
                });
                let started = *self.waiting.get_or_insert_with(Instant::now);
                if !paired && started.elapsed() < Duration::from_millis(25) {
                    break;
                }
                let directory = self.dirs.get(&event.wd).ok_or(DomainError::Internal)?;
                let path = directory.event_path(&event.name);
                if paired {
                    if event.cookie == 0 || self.moves.insert(event.cookie, path).is_some() {
                        return Err(DomainError::Internal);
                    }
                } else {
                    // read_events drained the kernel before this point. Tag
                    // older staged events before removing coverage. New reads
                    // from retired descriptors are discarded until IGNORED.
                    self.detach(&path)?;
                }
                self.waiting = None;
            }
            let event = self.pending.pop_front().unwrap();
            self.process(event, send, cancel)?;
        }
        Ok(())
    }

    fn bound_directory(&self, directory: &Directory) -> Result<Option<File>, DomainError> {
        let path = self.root_path.join(&directory.path);
        let path = CString::new(path.as_os_str().as_bytes()).map_err(|_| DomainError::Internal)?;
        let object = match owned_fd(unsafe {
            libc::open(
                path.as_ptr(),
                libc::O_PATH | libc::O_CLOEXEC | libc::O_DIRECTORY,
            )
        }) {
            Ok(object) => object,
            Err(error) if error.raw_os_error() == Some(libc::ENOENT) => return Ok(None),
            Err(error) => return Err(runtime_error(watch_error(error))),
        };
        let metadata = object
            .metadata()
            .map_err(|error| runtime_error(watch_error(error)))?;
        Ok((metadata.dev() == directory.dev && metadata.ino() == directory.ino).then_some(object))
    }

    fn detach(&mut self, path: &Path) -> Result<(), DomainError> {
        let removed: Vec<_> = self
            .dirs
            .iter()
            .filter(|(_, dir)| dir.path.starts_with(path))
            .map(|(wd, _)| *wd)
            .collect();
        for wd in removed {
            let dir = self.dirs.remove(&wd).unwrap();
            for event in &mut self.pending {
                if event.wd == wd {
                    event.detached = Some(dir.event_path(&event.name));
                }
            }
            self.retired.insert(wd);
            if unsafe { libc::inotify_rm_watch(self.fd.as_raw_fd(), wd) } < 0 {
                let error = std::io::Error::last_os_error();
                if error.raw_os_error() != Some(libc::EINVAL) {
                    return Err(runtime_error(watch_error(error)));
                }
            }
        }
        Ok(())
    }

    fn process(
        &mut self,
        event: RawEvent,
        send: &Sink,
        cancel: &AtomicBool,
    ) -> Result<(), DomainError> {
        if event.mask & libc::IN_Q_OVERFLOW != 0 {
            return Err(DomainError::InternalMessage(
                "watcher error: fsnotify: queue or buffer overflow".into(),
            ));
        }
        // IGNORED may already be in the batch annotated by detach(). Consume
        // retirement even then, without touching a newly registered directory.
        if event.mask & libc::IN_IGNORED != 0 && self.retired.remove(&event.wd) {
            return Ok(());
        }
        if self.retired.contains(&event.wd) && event.detached.is_none() {
            return Ok(());
        }
        if event.detached.is_some() && event.mask & (libc::IN_MOVE_SELF | libc::IN_DELETE_SELF) != 0
        {
            return Ok(());
        }
        let detached = event.detached.is_some();
        let path = if let Some(path) = event.detached {
            path
        } else {
            let Some(directory) = self.dirs.get(&event.wd) else {
                return Ok(());
            };
            let path = directory.event_path(&event.name);
            if event.mask & (libc::IN_UNMOUNT | libc::IN_IGNORED) != 0 {
                self.dirs.remove(&event.wd);
                return Ok(());
            }
            if event.mask & libc::IN_MOVE_SELF != 0 {
                if self.recursive && !directory.path.as_os_str().is_empty() {
                    return Ok(());
                }
                self.detach(&path)?;
            }
            if event.mask & libc::IN_DELETE_SELF != 0 {
                self.retired.insert(event.wd);
                self.dirs.remove(&event.wd);
                if !path.as_os_str().is_empty() {
                    return Ok(());
                }
            }
            path
        };
        if !detached
            && self.recursive
            && event.mask & libc::IN_ISDIR != 0
            && event.mask & libc::IN_DELETE != 0
        {
            self.detach(&path)?;
        }
        if !detached
            && self.recursive
            && event.mask & libc::IN_ISDIR != 0
            && event.mask & (libc::IN_CREATE | libc::IN_MOVED_TO) != 0
        {
            if let Some(old) = self.moves.remove(&event.cookie) {
                if !self.dirs.values().any(|dir| dir.path == old) {
                    return Err(DomainError::Internal);
                }
                for directory in self.dirs.values_mut() {
                    if let Ok(suffix) = directory.path.strip_prefix(&old) {
                        directory.path = if suffix.as_os_str().is_empty() {
                            path.clone()
                        } else {
                            path.join(suffix)
                        };
                    }
                }
                // Existing descriptors already cover this object and descendants.
                // A later queued rename may have removed this intermediate name.
            } else if let Some(parent) = self.bound_directory(&self.dirs[&event.wd])? {
                match open_at(&parent, &event.name, libc::O_DIRECTORY | libc::O_NOFOLLOW) {
                    Ok(child) => {
                        self.install(child, path.clone(), cancel, Some(send))
                            .map_err(runtime_error)?;
                        return Ok(());
                    }
                    Err(error) => return Err(runtime_error(watch_error(error))),
                }
            }
        }

        let name = if path.as_os_str().is_empty() {
            ".".into()
        } else {
            path.to_string_lossy().into_owned()
        };
        for (mask, kind) in [
            (libc::IN_CREATE | libc::IN_MOVED_TO, EventType::Create),
            (libc::IN_MOVED_FROM | libc::IN_MOVE_SELF, EventType::Rename),
            (libc::IN_ATTRIB, EventType::Chmod),
            (libc::IN_MODIFY, EventType::Write),
            (libc::IN_DELETE | libc::IN_DELETE_SELF, EventType::Remove),
        ] {
            if event.mask & mask != 0 {
                send.append(
                    FilesystemEvent {
                        name: name.clone(),
                        r#type: kind.into(),
                        ..Default::default()
                    },
                    cancel,
                )?;
            }
        }
        Ok(())
    }
}

fn exhausted() -> DomainError {
    DomainError::ResourceExhausted("watch resources exhausted".into())
}
fn watch_error(error: std::io::Error) -> DomainError {
    DomainError::InternalMessage(io_detail(&error))
}

// Linux filesystem magic values cover known unsupported remote and FUSE
// backends. A local bind mount retains its underlying filesystem type.
fn supported_backend(object: &File) -> Result<(), DomainError> {
    let mut info = std::mem::MaybeUninit::<libc::statfs>::uninit();
    if unsafe { libc::fstatfs(object.as_raw_fd(), info.as_mut_ptr()) } != 0 {
        return Err(watch_error(std::io::Error::last_os_error()));
    }
    let kind = unsafe { info.assume_init() }.f_type as u64;
    if matches!(
        kind,
        0x6969 // NFS
            | 0x517b // SMB
            | 0xff534d42 // CIFS
            | 0xfe534d42 // SMB2
            | 0x65735546 // FUSE
    ) {
        return Err(invalid("unsupported watch filesystem"));
    }
    Ok(())
}

fn runtime_error(error: DomainError) -> DomainError {
    match error {
        DomainError::Cancelled => error,
        _ => DomainError::InternalMessage(format!("watcher error: {}", error.public_message())),
    }
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! `process.Process` RPC implementations.
//!
//! Baseline-verified streaming semantics:
//! - the whole Start response is HTTP 200; all errors (bad user, deadline,
//!   unimplemented capability) travel as EndStream error frames;
//! - `Connect-Timeout-Ms` on Start is a process deadline (KILL +
//!   `deadline_exceeded`); on Connect it bounds only that attachment;
//! - a client disconnect does NOT kill the child — it keeps running and
//!   stays visible to `List` until it exits on its own.

use std::sync::Arc;

use bytes::Bytes;
use tokio::io::AsyncWriteExt;
use tokio::sync::broadcast::error::RecvError;
use tokio::sync::{broadcast, mpsc};

use crate::auth::User;
use crate::cgroup::{self, ProcType};
use crate::connect;
use crate::error::{ConnectCode, ConnectError};
use crate::exec;
use crate::msg::process::{
    parse_signal, CloseStdinRequest, ConnectRequest, Event, EventEnvelope, ListResponse,
    ProcessInfo, ProcessInput, ProcessSelector, SendInputRequest, SendSignalRequest, StartEvent,
    StartRequest, StreamInputRequest, UpdateRequest,
};
use crate::state::{AppState, ProcEntry, PtyResizeError};

const RESPONSE_QUEUE_CAPACITY: usize = 65;
const PROCESS_REAP_GRACE: std::time::Duration = std::time::Duration::from_secs(2);

pub fn frame_stream_response(
    frames: impl futures::Stream<Item = Bytes> + Send + 'static,
) -> axum::response::Response {
    use futures::StreamExt;
    let body = axum::body::Body::from_stream(frames.map(Ok::<_, std::convert::Infallible>));
    axum::response::Response::builder()
        .status(axum::http::StatusCode::OK)
        .header(
            axum::http::header::CONTENT_TYPE,
            connect::STREAM_CONTENT_TYPE,
        )
        .body(body)
        .expect("build stream response")
}

/// A streaming response that carries a single EndStream error frame.
pub fn stream_error_response(err: ConnectError) -> axum::response::Response {
    frame_stream_response(futures::stream::iter([connect::end_stream_error(&err)]))
}

/// Successful response for a client-streaming RPC: one empty response message
/// followed by the mandatory EndStream envelope.
pub fn empty_stream_response() -> axum::response::Response {
    let message = connect::message_frame(&serde_json::json!({}));
    let trailer = connect::end_stream_ok();
    let mut frames = Vec::with_capacity(message.len() + trailer.len());
    frames.extend_from_slice(&message);
    frames.extend_from_slice(&trailer);
    frame_stream_response(futures::stream::iter([Bytes::from(frames)]))
}

fn event_frame(event: Event) -> Bytes {
    let value =
        serde_json::to_value(EventEnvelope { event }).unwrap_or_else(|_| serde_json::json!({}));
    connect::message_frame(&value)
}

/// Target OOM score and nice level for user commands (upstream
/// handler.go:29-30). The wrapper applies them in the child before the
/// command exists, so nothing can inherit envd's protected values.
const DEFAULT_OOM_SCORE: i32 = 100;
const DEFAULT_NICE: i32 = 0;

/// Nice value of the current process, as the wrapper delta needs it.
/// Reads the raw syscall the way upstream does (handler.go:82-88): the
/// kernel encodes the result as `20 - nice`, so `20 - prio` is the nice
/// value. Reading the raw encoding rather than the C library's
/// `getpriority` is deliberate — libc converts to the nice value itself,
/// where `-1` is both a legitimate nice level and the error return, so a
/// daemon started at nice -1 could not be told apart from a failure. The
/// raw encoding maps every valid nice to `1..=40`, leaving -1 unambiguously
/// an error. (Do not apply `20 - prio` to a libc `getpriority` result: that
/// combination yields delta -20 at the daemon's nice 0 and the wrapper then
/// tries to raise priority as the unprivileged user, failing with EPERM.)
fn current_nice() -> i32 {
    // SAFETY: getpriority(PRIO_PROCESS, 0) addresses this process.
    let prio = unsafe { libc::syscall(libc::SYS_getpriority, libc::PRIO_PROCESS, 0) };
    if prio < 0 {
        return 0;
    }
    20 - prio as i32
}

/// Assemble the OOM/nice wrapper upstream prepends to every user command
/// (handler.go:98-105): `echo <oom> > /proc/$$/oom_score_adj && exec
/// /usr/bin/nice -n <delta> "$@"`, run as `/bin/sh -c <script> -- <cmd>
/// <args...>`. The command replaces sh via exec (same pid) after the score
/// and nice are set in the child. nice(1) is a relative adjustment, hence
/// the delta from the current (inherited) nice to the target.
fn oom_nice_wrapper(cmd: &str, args: &[String]) -> (String, Vec<String>) {
    let delta = DEFAULT_NICE - current_nice();
    let script = format!(
        "echo {DEFAULT_OOM_SCORE} > /proc/$$/oom_score_adj && exec /usr/bin/nice -n {delta} \"${{@}}\""
    );
    let mut sh_args = vec!["-c".to_string(), script, "--".to_string(), cmd.to_string()];
    sh_args.extend(args.iter().cloned());
    ("/bin/sh".to_string(), sh_args)
}

/// The user's original command as one shell-style string, wrapper internals
/// stripped — mirrors upstream `Handler.userCommand` (handler.go:71-74,
/// `strings.Join(append([cmd], args...), " ")`). No shell escaping: upstream
/// does none either; this text only feeds error messages.
fn user_command(cmd: &str, args: &[String]) -> String {
    std::iter::once(cmd)
        .chain(args.iter().map(String::as_str))
        .collect::<Vec<_>>()
        .join(" ")
}

/// Which cgroup subtree a Start request belongs to (upstream
/// handler.go getProcType). PTY commands use the higher-weight `ptys` subtree;
/// pipe commands use `user`.
pub(crate) fn get_proc_type(req: &StartRequest) -> ProcType {
    if req.pty.is_some() {
        ProcType::Pty
    } else {
        ProcType::User
    }
}

/// Handle `process.Process/Start`.
pub fn start(
    state: Arc<AppState>,
    req: StartRequest,
    user: User,
    deadline: Option<std::time::Duration>,
    keepalive_interval: std::time::Duration,
) -> axum::response::Response {
    if req.process.cmd.is_empty() {
        return stream_error_response(ConnectError::new(
            ConnectCode::InvalidArgument,
            "process config has no cmd",
        ));
    }

    let env = exec::merged_env(&state, &user, &req.process.envs);
    let cwd = match exec::resolve_cwd(req.process.cwd.as_deref(), &user) {
        Ok(c) => c,
        Err(msg) => {
            // Invalid working directory: reject like upstream instead of
            // silently running in `/` (#1227: no silent success).
            return stream_error_response(ConnectError::new(ConnectCode::InvalidArgument, msg));
        }
    };

    // Upstream wraps every start in an OOM/nice shell (handler.go:98-105):
    // the wrapper also makes a "missing binary" a wrapper-level failure with
    // /usr/bin/nice's own wording (exit 127 event flow), not a spawn error.
    let (wrapper_cmd, wrapper_args) = oom_nice_wrapper(&req.process.cmd, &req.process.args);
    let process_cgroup = match state.create_process_cgroup(get_proc_type(&req)) {
        Ok(cgroup) => cgroup,
        Err(e) => {
            return stream_error_response(ConnectError::new(
                ConnectCode::ResourceExhausted,
                format!("cannot allocate process cgroup: {e}"),
            ));
        }
    };
    // PTY vs pipe spawn differ only in the stdio plumbing; everything
    // downstream (broadcast pump → drive_stream) is identical because the pty
    // master publishes the same `DataEvent { pty }` onto the same bus.
    let spawn_once = |cgroup_fd, cgroup_for_spawn: Option<Arc<cgroup::ProcessCgroup>>| {
        if let Some(pty) = &req.pty {
            let (cols, rows) = pty
                .size
                .as_ref()
                .map(|s| (s.cols as u16, s.rows as u16))
                .unwrap_or((0, 0));
            exec::spawn_pty_with_cgroup(
                &wrapper_cmd,
                &wrapper_args,
                env.clone(),
                cwd.clone(),
                &user,
                (cols, rows),
                cgroup_fd,
                cgroup_for_spawn,
            )
        } else {
            // Backwards compatibility: an omitted Start.stdin defaults to true.
            exec::spawn_with_cgroup(
                &wrapper_cmd,
                &wrapper_args,
                env.clone(),
                cwd.clone(),
                &user,
                req.stdin.unwrap_or(true),
                cgroup_fd,
                cgroup_for_spawn,
            )
        }
    };
    let spawn_result = spawn_once(
        process_cgroup.as_deref().map(cgroup::ProcessCgroup::fd),
        process_cgroup.clone(),
    );
    let spawned = match spawn_result {
        Ok(s) => s,
        Err(e) => {
            if let Some(group) = &process_cgroup {
                if let Err(cleanup_error) = group.remove_if_empty() {
                    tracing::warn!("failed to remove unused process cgroup: {cleanup_error}");
                }
            }
            // A genuine spawn failure is an InvalidArgument RPC error. The
            // wrapper makes a missing user command a natural exit-127 event.
            return stream_error_response(ConnectError::new(
                ConnectCode::InvalidArgument,
                format!(
                    "error starting process '{}': {e}",
                    user_command(&req.process.cmd, &req.process.args)
                ),
            ));
        }
    };

    let exec::SpawnedProcess {
        pid,
        initial,
        sender,
        pty_master,
        input,
        completion,
        terminal,
        reaped,
        termination,
        cgroup: spawned_cgroup,
    } = spawned;
    debug_assert_eq!(
        spawned_cgroup
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .as_ref()
            .map(|group| group.path()),
        process_cgroup.as_ref().map(|group| group.path()),
        "spawn pump must receive cgroup metadata before it starts"
    );
    let supervisor_sender = sender.clone();
    let supervisor_cgroup = process_cgroup.clone();
    let supervisor_reaped = reaped.clone();
    let handle = state.insert_process(ProcEntry {
        pid,
        tag: req.tag.clone(),
        config: req.process.clone(),
        sender,
        pty_master,
        input,
        cgroup: process_cgroup,
        termination: termination.clone(),
        terminal,
    });
    let supervisor_state = state.clone();
    tokio::spawn(async move {
        supervise_process(
            supervisor_state,
            handle,
            pid,
            supervisor_sender,
            completion,
            deadline,
            supervisor_cgroup,
            supervisor_reaped,
            termination.clone(),
        )
        .await;
    });

    // Frames channel: the HTTP body reads from `rx`. The driver never waits
    // for capacity here: a slow client must not prevent deadline handling or
    // process reaping.
    // Keep one slot reserved for an EndStream frame. At most 64 ordinary
    // frames may be queued, so a client that falls behind still receives an
    // explicit resource_exhausted trailer once it resumes reading.
    let (tx, rx) = mpsc::channel::<Bytes>(RESPONSE_QUEUE_CAPACITY);
    tokio::spawn(async move {
        drive_stream(pid, initial, tx, keepalive_interval, None).await;
    });

    frame_stream_response(tokio_stream::wrappers::ReceiverStream::new(rx))
}

/// Handle `process.Process/Connect` (attach, server-streaming).
///
/// Attach differs from `Start` only in lifecycle: it does not spawn and never
/// owns the process. The independent supervisor handles deadline and table
/// cleanup, so disconnecting either stream only releases that subscription.
pub fn connect(
    state: Arc<AppState>,
    req: ConnectRequest,
    keepalive_interval: std::time::Duration,
    stream_deadline: Option<std::time::Duration>,
) -> axum::response::Response {
    let (pid, tag) = match validated_selector(&req.process) {
        Ok(selector) => selector,
        Err(e) => return stream_error_response(e),
    };
    let Some((pid, events)) = state.subscribe(pid, tag.as_deref()) else {
        // Match Go envd's wording for a selector resolving to no live process
        // (the same helper SendSignal/List use).
        return stream_error_response(not_found(pid, tag.as_deref()));
    };

    // The fresh receiver starts at the current ring head, so history is not
    // replayed.
    let (tx, rx) = mpsc::channel::<Bytes>(RESPONSE_QUEUE_CAPACITY);
    tokio::spawn(async move {
        drive_stream(pid, events, tx, keepalive_interval, stream_deadline).await;
    });

    frame_stream_response(tokio_stream::wrappers::ReceiverStream::new(rx))
}

#[allow(clippy::too_many_arguments)]
async fn supervise_process(
    state: Arc<AppState>,
    handle: crate::state::ProcHandle,
    pid: u32,
    sender: broadcast::Sender<exec::PumpEvent>,
    mut completion: tokio::sync::oneshot::Receiver<()>,
    deadline: Option<std::time::Duration>,
    cgroup: Option<Arc<cgroup::ProcessCgroup>>,
    reaped: Arc<tokio::sync::Notify>,
    termination: Arc<std::sync::Mutex<Option<String>>>,
) {
    if let Some(deadline) = deadline {
        let reaped_signal = reaped.notified();
        tokio::pin!(reaped_signal);
        tokio::select! {
            // Prefer a direct-child reap that became ready at the same instant
            // as the deadline; a process that already exited is not timed out
            // merely because output-drain grace is still running.
            biased;
            _ = &mut reaped_signal => {
                let result = tokio::time::timeout(PROCESS_REAP_GRACE, &mut completion).await;
                let monitor_ok = matches!(result, Ok(Ok(())));
                if !monitor_ok {
                    let _ = kill_process_tree(pid, cgroup.as_ref());
                    let error = exec::PumpEvent::SpawnError(
                        "process monitor stopped before reporting exit".into(),
                    );
                    state.mark_terminal(handle, error.clone());
                    state.remove_process(handle);
                    let _ = sender.send(error);
                } else {
                    state.remove_process(handle);
                }
                if monitor_ok {
                    kill_descendants_and_cleanup(pid, cgroup).await;
                } else {
                    cleanup_process_cgroup(cgroup).await;
                }
            }
            _ = tokio::time::sleep(deadline) => {
                // Remove first so a concurrent Connect/Input/Update cannot
                // attach to a command whose deadline has already expired.
                state.remove_process(handle);
                // Publish the deadline marker before signalling the child.
                // cgroup.kill can make the pump race to publish End on another
                // runtime worker; ordering this event first guarantees every
                // still-attached stream observes deadline_exceeded rather than
                // a misleading normal End.
                let _ = sender.send(exec::PumpEvent::DeadlineExceeded);
                // Serialize the cause marker with the kill syscall. SendSignal
                // uses the same mutex; holding it across both operations means
                // a concurrent user signal cannot race with timeout and leave
                // `killedBy` describing the wrong operation.
                let kill_result = {
                    let mut cause = termination
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner);
                    // Publish a tentative cause before the syscall while the
                    // same mutex is held.  The output pump also takes this
                    // mutex before decorating EndEvent, so it cannot observe
                    // a successful kill in the gap before the marker is
                    // written.  If the process already exited (ESRCH), clear
                    // the tentative value so a natural exit is not mislabeled.
                    *cause = Some("timeout".to_string());
                    let result = kill_process_tree(pid, cgroup.as_ref());
                    if result.is_err() {
                        *cause = None;
                    }
                    result
                };
                if let Err(e) = kill_result {
                    if e.raw_os_error() != Some(libc::ESRCH) {
                        tracing::warn!("pid {pid}: deadline kill failed: {e}");
                    }
                }
                // Retain supervision while streams await the child's EndEvent.
                // The direct child must actually be reaped. Bound this wait
                // so a failed/denied kill cannot leak the supervisor forever.
                if tokio::time::timeout(PROCESS_REAP_GRACE, &mut completion)
                    .await
                    .is_err()
                {
                    tracing::warn!(
                        "pid {pid}: timed out waiting for direct child reap after deadline kill"
                    );
                }
                cleanup_process_cgroup(cgroup).await;
            }
        }
    } else {
        let monitor_ok = completion.await.is_ok();
        if !monitor_ok {
            let _ = kill_process_tree(pid, cgroup.as_ref());
            let error =
                exec::PumpEvent::SpawnError("process monitor stopped before reporting exit".into());
            state.mark_terminal(handle, error.clone());
            state.remove_process(handle);
            let _ = sender.send(error);
        } else {
            state.remove_process(handle);
        }
        if monitor_ok {
            kill_descendants_and_cleanup(pid, cgroup).await;
        } else {
            cleanup_process_cgroup(cgroup).await;
        }
    }
}

/// Kill the direct process group, using the per-command cgroup as the stronger
/// mechanism when available. cgroup.kill is the only operation that reaches a
/// descendant which called setsid(); a missing/failed cgroup falls back to the
/// established process-group signal.
fn kill_process_tree(pid: u32, cgroup: Option<&Arc<cgroup::ProcessCgroup>>) -> std::io::Result<()> {
    if let Some(cgroup) = cgroup {
        match cgroup.kill_all() {
            Ok(()) => return Ok(()),
            Err(e) if e.raw_os_error() == Some(libc::ESRCH) => {
                // An empty per-command leaf is authoritative: the direct
                // child has already gone away. Do not fall back to
                // kill(-pid), whose process-group id may have been recycled
                // for an unrelated process.
                return Err(e);
            }
            Err(e) => {
                tracing::warn!("pid {pid}: cgroup.kill failed, falling back to process group: {e}")
            }
        }
    }
    exec::kill_process_group(pid, libc::SIGKILL)
}

async fn kill_descendants_and_cleanup(pid: u32, cgroup: Option<Arc<cgroup::ProcessCgroup>>) {
    let cgroup_result = if let Some(cgroup_ref) = cgroup.as_ref() {
        match cgroup_ref.kill_all() {
            Ok(()) => Some(true),
            Err(e) if e.raw_os_error() == Some(libc::ESRCH) => {
                // Empty cgroup means there are no descendants to clean. In
                // particular, do not signal a potentially recycled pgid.
                Some(false)
            }
            Err(e) => {
                tracing::warn!("pid {pid}: cgroup.kill after exit failed: {e}");
                None
            }
        }
    } else {
        None
    };

    // A Noop/degraded cgroup manager still needs to reap descendants that
    // stayed in the original process group. Only cgroup.kill reaches a
    // setsid() escapee; when no cgroup is available, retain the established
    // process-group fallback for ordinary descendants.
    if cgroup_result.is_none() {
        if let Err(group_error) = exec::kill_process_group(pid, libc::SIGKILL) {
            if group_error.raw_os_error() != Some(libc::ESRCH) {
                tracing::warn!("pid {pid}: process-group descendant cleanup failed: {group_error}");
            }
        }
    }
    cleanup_process_cgroup(cgroup).await;
}

async fn cleanup_process_cgroup(cgroup: Option<Arc<cgroup::ProcessCgroup>>) {
    let Some(cgroup) = cgroup else {
        return;
    };
    let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(2);
    loop {
        match cgroup.remove_if_empty() {
            Ok(()) => return,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                if tokio::time::Instant::now() >= deadline {
                    tracing::warn!(
                        "cgroup: timed out removing process leaf {}",
                        cgroup.path().display()
                    );
                    return;
                }
                tokio::time::sleep(std::time::Duration::from_millis(10)).await;
            }
            Err(e) => {
                tracing::warn!(
                    "cgroup: failed to remove process leaf {}: {e}",
                    cgroup.path().display()
                );
                return;
            }
        }
    }
}

async fn drive_stream(
    pid: u32,
    mut events: broadcast::Receiver<exec::PumpEvent>,
    tx: mpsc::Sender<Bytes>,
    keepalive_interval: std::time::Duration,
    stream_deadline: Option<std::time::Duration>,
) {
    // The producer is dropped immediately on backpressure or disconnect.
    // Process lifetime is owned by the separate supervisor, so this task
    // never needs to retain a dead HTTP client's broadcast subscription.
    let mut output = Some(tx);
    let mut deadline_seen = false;
    if !try_send_data_frame(&mut output, event_frame(Event::Start(StartEvent { pid }))) {
        return;
    }

    let mut keepalive = tokio::time::interval(keepalive_interval);
    keepalive.reset(); // first tick fires after one period, not immediately
    let deadline = async move {
        match stream_deadline {
            Some(deadline) => tokio::time::sleep(deadline).await,
            None => std::future::pending::<()>().await,
        }
    };
    tokio::pin!(deadline);

    loop {
        let close_signal = output.as_ref().cloned().expect("output sender is live");
        tokio::select! {
            _ = close_signal.closed() => return,
            ev = events.recv() => match ev {
                Ok(exec::PumpEvent::Data(d)) => {
                    keepalive.reset();
                    if !try_send_data_frame(&mut output, event_frame(Event::Data(d))) {
                        return;
                    }
                }
                Ok(exec::PumpEvent::End(end)) => {
                    // One queue slot carries both terminal envelopes. This
                    // prevents a nearly-full queue from exposing End without
                    // the required EndStream trailer.
                    let event = event_frame(Event::End(end));
                    let trailer = if deadline_seen {
                        connect::end_stream_error(&ConnectError::new(
                            ConnectCode::DeadlineExceeded,
                            "context deadline exceeded",
                        ))
                    } else {
                        connect::end_stream_ok()
                    };
                    let mut terminal = Vec::with_capacity(event.len() + trailer.len());
                    terminal.extend_from_slice(&event);
                    terminal.extend_from_slice(&trailer);
                    try_send_terminal_frame(&mut output, Bytes::from(terminal));
                    return;
                }
                Ok(exec::PumpEvent::SpawnError(msg)) => {
                    try_send_terminal_frame(&mut output, connect::end_stream_error(&ConnectError::new(
                        ConnectCode::Internal,
                        msg,
                    )));
                    return;
                }
                Ok(exec::PumpEvent::DeadlineExceeded) => {
                    // The supervisor has recorded the timeout and started
                    // killing the process. Keep this attachment alive until
                    // the pump publishes the real EndEvent, so clients get
                    // both the actual signal and `killedBy: "timeout"`.
                    deadline_seen = true;
                }
                Err(RecvError::Lagged(n)) => {
                    try_send_terminal_frame(&mut output, connect::end_stream_error(&ConnectError::new(
                        ConnectCode::ResourceExhausted,
                        format!("output consumer too slow: {n} events dropped"),
                    )));
                    return;
                }
            Err(RecvError::Closed) => {
                    let (code, message) = if deadline_seen {
                        (ConnectCode::DeadlineExceeded, "context deadline exceeded")
                    } else {
                        (
                            ConnectCode::Internal,
                            "process output stream closed before a terminal event",
                        )
                    };
                    try_send_terminal_frame(
                        &mut output,
                        connect::end_stream_error(&ConnectError::new(code, message)),
                    );
                    return;
                }
            },
            _ = keepalive.tick() => {
                if !try_send_data_frame(&mut output, event_frame(Event::KeepAlive(serde_json::Map::new()))) {
                    return;
                }
            }
            _ = &mut deadline, if !deadline_seen => {
                // A Connect timeout bounds this attachment only. The process
                // belongs to its Start supervisor and must remain available
                // for List, input and a later Connect.
                try_send_terminal_frame(&mut output, connect::end_stream_error(&ConnectError::new(
                    ConnectCode::DeadlineExceeded,
                    "context deadline exceeded",
                )));
                return;
            }
        }
    }
}

/// Never await HTTP response capacity from the process driver. One queue slot
/// is reserved for the terminal error: when ordinary output reaches that
/// boundary, close only this connection with an explicit resource_exhausted
/// EndStream frame while the independent supervisor continues deadline and
/// process cleanup work.
fn try_send_data_frame(output: &mut Option<mpsc::Sender<Bytes>>, frame: Bytes) -> bool {
    let Some(tx) = output.as_ref() else {
        return false;
    };
    if tx.capacity() <= 1 {
        try_send_terminal_frame(
            output,
            connect::end_stream_error(&ConnectError::new(
                ConnectCode::ResourceExhausted,
                "output consumer too slow: response queue full",
            )),
        );
        return false;
    }

    if tx.try_send(frame).is_err() {
        output.take();
        return false;
    }
    true
}

/// Queue a final EndStream-bearing frame in the reserved slot and drop the
/// producer. A closed receiver needs no trailer because the HTTP client is
/// already gone.
fn try_send_terminal_frame(output: &mut Option<mpsc::Sender<Bytes>>, frame: Bytes) -> bool {
    let sent = output.as_ref().is_some_and(|tx| tx.try_send(frame).is_ok());
    output.take();
    sent
}

/// Handle `process.Process/List` (unary).
pub fn list(state: &AppState) -> serde_json::Value {
    let processes = state
        .list_processes()
        .into_iter()
        .map(|(pid, tag, config)| ProcessInfo {
            config,
            pid: Some(pid),
            tag,
        })
        .collect();
    serde_json::to_value(ListResponse { processes }).unwrap_or_else(|_| serde_json::json!({}))
}

/// Handle `process.Process/SendSignal` (unary).
pub fn send_signal(
    state: &AppState,
    req: &SendSignalRequest,
) -> Result<serde_json::Value, ConnectError> {
    let (pid, tag) = validated_selector(&req.process)?;
    let (target, process_cgroup, termination) =
        state.process_control(pid, tag.as_deref()).ok_or_else(|| {
            // Match Go envd's specific wording so a client logging the message
            // sees the same text: "process with pid N not found" / "... tag X ...".
            not_found(pid, tag.as_deref())
        })?;
    let signo = parse_signal(req.signal.as_ref()).ok_or_else(|| {
        ConnectError::new(
            ConnectCode::InvalidArgument,
            format!("unsupported signal: {:?}", req.signal),
        )
    })?;
    // Serialize the signal syscall and the termination-cause marker. The
    // output pump takes the same mutex while decorating EndEvent, so it cannot
    // publish a natural exit in the gap between a successful kill and writing
    // `killedBy: "user"`. Conversely, an ESRCH leaves no stale user marker and
    // the natural exit remains correctly classified.
    // The table can still hold a pid whose process exited but was not yet
    // reaped; kill(-pid) then fails with ESRCH. Report that as not_found
    // (the process is gone from the caller's perspective, matching Go),
    // not a misleading Internal.
    let result = {
        let mut cause = termination
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        // As with timeout supervision, hold the cause marker across the
        // signal syscall.  A failed/ESRCH signal must not leave stale
        // `killedBy: "user"` metadata on a natural exit.
        *cause = Some("user".to_string());
        let result = if signo == libc::SIGKILL {
            kill_process_tree(target, process_cgroup.as_ref())
        } else if let Some(cgroup) = process_cgroup.as_ref() {
            // cgroup v2 has no generic "signal every task" primitive, but
            // enumerating the leaf closes the setsid() escape that a bare
            // process-group signal leaves behind. A transient cgroupfs/read
            // failure falls back to the established process-group behavior so a
            // degraded hierarchy never makes SendSignal unavailable.
            match cgroup.signal_all(signo) {
                Ok(()) => Ok(()),
                Err(e) => {
                    tracing::warn!(
                        "pid {target}: cgroup signal failed, falling back to process group: {e}"
                    );
                    exec::kill_process_group(target, signo)
                }
            }
        } else {
            exec::kill_process_group(target, signo)
        };
        if result.is_err() {
            *cause = None;
        }
        result
    };
    result.map_err(|e| {
        if e.raw_os_error() == Some(libc::ESRCH) {
            not_found(pid, tag.as_deref())
        } else {
            ConnectError::new(ConnectCode::Internal, format!("kill failed: {e}"))
        }
    })?;
    Ok(serde_json::json!({}))
}

/// Handle `process.Process/SendInput` (unary). Input handles are process-owned
/// and mutex-protected, so concurrent unary calls and StreamInput messages are
/// written in a deterministic order without serializing unrelated processes.
pub async fn send_input(
    state: &AppState,
    req: &SendInputRequest,
) -> Result<serde_json::Value, ConnectError> {
    let (pid, tag) = validated_selector(&req.process)?;
    let input = state
        .input_handle(pid, tag.as_deref())
        .ok_or_else(|| not_found(pid, tag.as_deref()))?;
    write_process_input(pid, &input, &req.input).await?;
    Ok(serde_json::json!({}))
}

/// Apply one StreamInput event in receive order. A `start` event selects (or
/// reselects) the target process; data before selection is rejected rather than
/// dereferencing an absent writer, and keepalive is intentionally a no-op.
pub async fn stream_input_event(
    state: &AppState,
    selected: &mut Option<exec::InputHandle>,
    req: StreamInputRequest,
) -> Result<(), ConnectError> {
    let arms = usize::from(req.start.is_some())
        + usize::from(req.data.is_some())
        + usize::from(req.keepalive.is_some());
    if arms != 1 {
        return Err(ConnectError::new(
            ConnectCode::Unimplemented,
            "invalid event type <nil>",
        ));
    }

    if let Some(start) = req.start {
        let (pid, tag) = validated_selector(&start.process)?;
        *selected = Some(
            state
                .input_handle(pid, tag.as_deref())
                .ok_or_else(|| not_found(pid, tag.as_deref()))?,
        );
    } else if let Some(data) = req.data {
        let input = selected.as_ref().ok_or_else(|| {
            ConnectError::new(
                ConnectCode::InvalidArgument,
                "input stream has no process selected",
            )
        })?;
        write_process_input(None, input, &data.input).await?;
    }
    Ok(())
}

/// Close stdin. Taking a pipe writer and shutting it down delivers EOF to the
/// child. A PTY has no separate closeable stdin stream; callers must send
/// Ctrl-D through SendInput instead.
pub async fn close_stdin(
    state: &AppState,
    req: &CloseStdinRequest,
) -> Result<serde_json::Value, ConnectError> {
    let (pid, tag) = validated_selector(&req.process)?;
    let input = state
        .input_handle(pid, tag.as_deref())
        .ok_or_else(|| not_found(pid, tag.as_deref()))?;
    let mut writer = input.lock().await;
    match &mut *writer {
        exec::InputWriter::Pty(_) => Err(ConnectError::new(
            ConnectCode::Unknown,
            "error closing stdin: cannot close stdin for PTY process — send Ctrl+D (0x04) instead",
        )),
        exec::InputWriter::Pipe(pipe) => {
            if let Some(mut stdin) = pipe.take() {
                stdin.shutdown().await.map_err(|e| {
                    ConnectError::new(ConnectCode::Unknown, format!("error closing stdin: {e}"))
                })?;
            }
            Ok(serde_json::json!({}))
        }
    }
}

async fn write_process_input(
    pid: Option<u32>,
    input: &exec::InputHandle,
    request: &ProcessInput,
) -> Result<(), ConnectError> {
    use base64::Engine;

    let (kind, encoded) = match (&request.stdin, &request.pty) {
        (Some(data), None) => (InputKind::Stdin, data),
        (None, Some(data)) => (InputKind::Pty, data),
        _ => {
            return Err(ConnectError::new(
                ConnectCode::Unimplemented,
                "invalid input type <nil>",
            ))
        }
    };
    let data = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .map_err(|e| {
            ConnectError::new(
                ConnectCode::InvalidArgument,
                format!("invalid base64 process input: {e}"),
            )
        })?;

    let mut writer = input.lock().await;
    match (&mut *writer, kind) {
        (exec::InputWriter::Pty(pty), InputKind::Pty) => {
            exec::write_pty(pty, &data).await.map_err(|e| {
                ConnectError::new(ConnectCode::Internal, format!("error writing to tty: {e}"))
            })
        }
        (exec::InputWriter::Pty(_), InputKind::Stdin) => Err(ConnectError::new(
            ConnectCode::Internal,
            "error writing to stdin: tty assigned to process — input should be written to the pty, not the stdin",
        )),
        (exec::InputWriter::Pipe(_), InputKind::Pty) => Err(ConnectError::new(
            ConnectCode::Internal,
            "error writing to tty: tty not assigned to process — input should be written to the stdin, not the tty",
        )),
        (exec::InputWriter::Pipe(Some(stdin)), InputKind::Stdin) => {
            stdin.write_all(&data).await.map_err(|e| {
                ConnectError::new(
                    ConnectCode::Internal,
                    format!(
                        "error writing to stdin: {}",
                        match pid {
                            Some(pid) => format!("error writing to stdin of process '{pid}': {e}"),
                            None => e.to_string(),
                        }
                    ),
                )
            })
        }
        (exec::InputWriter::Pipe(None), InputKind::Stdin) => Err(ConnectError::new(
            ConnectCode::Internal,
            "error writing to stdin: stdin not enabled or closed",
        )),
    }
}

#[derive(Clone, Copy)]
enum InputKind {
    Stdin,
    Pty,
}

/// The "not found" detail shared by every selector-based RPC — byte-for-byte
/// the Go envd wording a client logs ("process with pid N not found" /
/// "... tag X ..." / bare "process not found").
fn not_found(pid: Option<u32>, tag: Option<&str>) -> ConnectError {
    let detail = match (pid, tag) {
        (Some(p), _) => format!("process with pid {p} not found"),
        (None, Some(t)) => format!("process with tag {t} not found"),
        (None, None) => "process not found".to_string(),
    };
    ConnectError::new(ConnectCode::NotFound, detail)
}

fn validated_selector(
    selector: &ProcessSelector,
) -> Result<(Option<u32>, Option<String>), ConnectError> {
    match (selector.pid, selector.tag.as_deref()) {
        (Some(_), Some(_)) => Err(ConnectError::new(
            ConnectCode::InvalidArgument,
            "process selector cannot contain both pid and tag",
        )),
        (None, None) => Err(ConnectError::new(
            ConnectCode::Unimplemented,
            "invalid input type *process.ProcessSelector",
        )),
        _ => Ok(selector.flatten()),
    }
}

/// Handle `process.Process/Update` (unary): resize a live process's pty window.
///
/// Baseline-verified semantics (Go envd):
/// - the process is resolved FIRST, so an unknown selector is `not_found` even
///   when the `pty` field is absent;
/// - a missing `pty` (or a `pty` without a `size`) is a silent no-op success
///   (`{}`), never an error — the SDK always sends both, but a bare Update is
///   not a caller bug;
/// - a live process without a pty answers `internal` with Go's exact
///   "error resizing tty: ..." wording.
pub fn update(state: &AppState, req: &UpdateRequest) -> Result<serde_json::Value, ConnectError> {
    let (pid, tag) = validated_selector(&req.process)?;
    let Some(size) = req.pty.as_ref().and_then(|p| p.size.as_ref()) else {
        // Nothing to resize: resolve to keep the not_found contract, then no-op.
        state
            .find_pid(pid, tag.as_deref())
            .ok_or_else(|| not_found(pid, tag.as_deref()))?;
        return Ok(serde_json::json!({}));
    };
    let cols = size.cols as u16;
    let rows = size.rows as u16;
    match state.resize_pty(pid, tag.as_deref(), cols, rows) {
        Ok(()) => Ok(serde_json::json!({})),
        Err(PtyResizeError::NotFound) => Err(not_found(pid, tag.as_deref())),
        Err(PtyResizeError::NotAPty) => Err(ConnectError::new(
            ConnectCode::Internal,
            "error resizing tty: tty not assigned to process",
        )),
        Err(PtyResizeError::Io(e)) => Err(ConnectError::new(
            ConnectCode::Internal,
            format!("error resizing tty: {e}"),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn allocation_and_placement_errors_never_execute_user_code() {
        struct FailingManager(Option<Arc<cgroup::ProcessCgroup>>);
        impl cgroup::Manager for FailingManager {
            fn fd(&self, _kind: ProcType) -> Option<std::os::fd::RawFd> {
                None
            }

            fn create_process(
                &self,
                _kind: ProcType,
            ) -> std::io::Result<Option<Arc<cgroup::ProcessCgroup>>> {
                match &self.0 {
                    Some(group) => Ok(Some(group.clone())),
                    None => Err(std::io::Error::from_raw_os_error(libc::ENOSPC)),
                }
            }
        }

        let directory = tempfile::tempdir().unwrap();
        let marker = directory.path().join("must-not-execute");
        let invalid_group = Arc::new(cgroup::ProcessCgroup::new(
            directory.path().to_path_buf(),
            std::fs::File::open("/dev/null").unwrap(),
        ));
        for (group, code) in [
            (None, "resource_exhausted"),
            (Some(invalid_group), "invalid_argument"),
        ] {
            let state = Arc::new(AppState::new().with_cgroup(Arc::new(FailingManager(group))));
            for _ in 0..2 {
                let request: StartRequest = serde_json::from_value(serde_json::json!({
                    "process": {"cmd": "/usr/bin/touch", "args": [marker.to_str().unwrap()]},
                }))
                .unwrap();
                let response = start(
                    state.clone(),
                    request,
                    current_user(),
                    None,
                    std::time::Duration::from_secs(30),
                );
                let body = axum::body::to_bytes(response.into_body(), 4096)
                    .await
                    .unwrap();
                assert_eq!(body[0], connect::END_STREAM_FLAG);
                let payload: serde_json::Value = serde_json::from_slice(&body[5..]).unwrap();
                assert_eq!(payload["error"]["code"], code);
                assert!(!marker.exists());
            }
        }
    }

    fn disabled_input() -> exec::InputHandle {
        Arc::new(tokio::sync::Mutex::new(exec::InputWriter::Pipe(None)))
    }

    fn current_user() -> User {
        let uid = unsafe { libc::getuid() };
        let gid = unsafe { libc::getgid() };
        User {
            name: "test".into(),
            uid,
            gid,
            home: "/tmp".into(),
            groups: vec![gid],
        }
    }

    #[allow(clippy::type_complexity)]
    fn insert_spawned(
        state: &AppState,
        spawned: exec::SpawnedProcess,
    ) -> (
        u32,
        crate::state::ProcHandle,
        broadcast::Receiver<exec::PumpEvent>,
        tokio::sync::oneshot::Receiver<()>,
        broadcast::Sender<exec::PumpEvent>,
        Arc<tokio::sync::Notify>,
        Arc<std::sync::Mutex<Option<String>>>,
    ) {
        let exec::SpawnedProcess {
            pid,
            initial,
            sender,
            pty_master,
            input,
            completion,
            terminal,
            reaped,
            termination,
            cgroup,
        } = spawned;
        let supervisor_sender = sender.clone();
        let handle = state.insert_process(ProcEntry {
            pid,
            tag: None,
            config: crate::msg::process::ProcessConfig::default(),
            sender,
            pty_master,
            input,
            cgroup: cgroup
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .clone(),
            termination: termination.clone(),
            terminal,
        });
        (
            pid,
            handle,
            initial,
            completion,
            supervisor_sender,
            reaped,
            termination,
        )
    }

    #[test]
    fn list_shape() {
        let state = AppState::new();
        assert_eq!(list(&state), serde_json::json!({}));
        let (sender, _rx) = broadcast::channel::<exec::PumpEvent>(1);
        state.insert_process(ProcEntry {
            pid: 7,
            tag: Some("t".into()),
            config: crate::msg::process::ProcessConfig {
                cmd: "/bin/bash".into(),
                args: vec!["-c".into(), "x".into()],
                ..Default::default()
            },
            sender,
            pty_master: None,
            input: disabled_input(),
            cgroup: None,
            termination: Arc::new(std::sync::Mutex::new(None)),
            terminal: Arc::new(std::sync::Mutex::new(None)),
        });
        let v = list(&state);
        assert_eq!(v["processes"][0]["pid"], 7);
        assert_eq!(v["processes"][0]["tag"], "t");
        assert_eq!(v["processes"][0]["config"]["cmd"], "/bin/bash");
    }

    #[test]
    fn send_signal_unknown_process() {
        let state = AppState::new();
        let req: SendSignalRequest =
            serde_json::from_str(r#"{"process":{"tag":"nope"},"signal":"SIGNAL_SIGKILL"}"#)
                .unwrap();
        let err = send_signal(&state, &req).unwrap_err();
        assert_eq!(err.code, ConnectCode::NotFound);
    }

    #[test]
    fn selector_rejects_pid_and_tag_together() {
        let selector: ProcessSelector = serde_json::from_str(r#"{"pid":7,"tag":"shell"}"#).unwrap();
        let err = validated_selector(&selector).unwrap_err();
        assert_eq!(err.code, ConnectCode::InvalidArgument);
        assert!(err.message.contains("both pid and tag"));
    }

    #[test]
    fn selector_rejects_empty_oneof() {
        let selector: ProcessSelector = serde_json::from_str(r#"{}"#).unwrap();
        let err = validated_selector(&selector).unwrap_err();
        assert_eq!(err.code, ConnectCode::Unimplemented);
        assert!(err.message.contains("invalid input type"));
    }

    #[test]
    fn malformed_nested_selector_cannot_reach_control_dispatch() {
        // Decode before entering any selector-based service. This is the
        // important side-effect boundary: malformed input must be rejected,
        // never normalized to an empty selector or combined with a valid pid.
        assert!(
            serde_json::from_str::<ProcessSelector>(r#"{"selector":{"pid":7},"pid":8}"#).is_err()
        );
    }

    #[test]
    fn oom_nice_wrapper_shape() {
        // Script structure must match upstream handler.go:98-105: OOM score
        // written first, then exec /usr/bin/nice with a relative delta.
        let (cmd, args) = oom_nice_wrapper("/no/such/bin", &[]);
        assert_eq!(cmd, "/bin/sh");
        assert_eq!(args.len(), 4);
        assert_eq!(args[0], "-c");
        assert!(args[1].starts_with("echo 100 > /proc/$$/oom_score_adj && exec /usr/bin/nice -n "));
        assert!(args[1].ends_with(" \"${@}\""));
        assert_eq!(args[2], "--");
        assert_eq!(args[3], "/no/such/bin");

        let (_, args) = oom_nice_wrapper("/bin/echo", &["hi".to_string()]);
        assert_eq!(args[3], "/bin/echo");
        assert_eq!(args[4], "hi");
    }

    #[test]
    fn current_nice_returns_the_nice_value() {
        // Cross-check against libc's getpriority, which converts the kernel's
        // raw `20 - nice` encoding back to the nice value itself — an
        // independent source for the same number. This locks the convention:
        // returning the raw encoding (or applying `20 - prio` to the libc
        // result) both yield 20 at the daemon's nice 0, i.e. delta -20 and a
        // wrapper that tries to raise priority as the unprivileged user.
        // SAFETY: PRIO_PROCESS with pid 0 addresses this process.
        let libc_nice = unsafe { libc::getpriority(libc::PRIO_PROCESS, 0) };
        if libc_nice == -1 {
            // Ambiguous (real nice -1 vs error) and unreachable here: the
            // test process inherits nice 0.
            return;
        }
        assert_eq!(current_nice(), libc_nice);
    }

    #[test]
    fn user_command_joins_cmd_and_args_like_upstream() {
        // Mirrors handler.go:71-74: plain space join of cmd + args, no shell
        // escaping — the text is only for error messages.
        assert_eq!(user_command("sleep", &[]), "sleep");
        assert_eq!(user_command("sleep", &["30".to_string()]), "sleep 30");
        assert_eq!(
            user_command("/bin/echo", &["a b".to_string(), "c".to_string()]),
            "/bin/echo a b c"
        );
    }

    #[test]
    fn get_proc_type_maps_pty_and_default() {
        let base = StartRequest {
            process: crate::msg::process::ProcessConfig {
                cmd: "x".into(),
                ..Default::default()
            },
            pty: None,
            tag: None,
            stdin: None,
        };
        assert_eq!(get_proc_type(&base), ProcType::User);
        let pty_req = StartRequest {
            pty: Some(crate::msg::process::Pty { size: None }),
            ..base
        };
        assert_eq!(get_proc_type(&pty_req), ProcType::Pty);
    }

    #[test]
    fn update_missing_pty_resolves_then_noops() {
        let state = AppState::new();
        // Missing pty on an unknown process is still not_found (resolve first).
        let req: UpdateRequest = serde_json::from_str(r#"{"process":{"pid":1}}"#).unwrap();
        assert_eq!(
            update(&state, &req).unwrap_err().code,
            ConnectCode::NotFound
        );
        // Missing pty on a live process is a silent no-op success, not an error.
        let (sender, _rx) = broadcast::channel::<exec::PumpEvent>(1);
        state.insert_process(ProcEntry {
            pid: 7,
            tag: Some("t".into()),
            config: crate::msg::process::ProcessConfig::default(),
            sender,
            pty_master: None,
            input: disabled_input(),
            cgroup: None,
            termination: Arc::new(std::sync::Mutex::new(None)),
            terminal: Arc::new(std::sync::Mutex::new(None)),
        });
        let req: UpdateRequest = serde_json::from_str(r#"{"process":{"pid":7}}"#).unwrap();
        assert_eq!(update(&state, &req).unwrap(), serde_json::json!({}));
    }

    #[test]
    fn update_unknown_process() {
        let state = AppState::new();
        let req: UpdateRequest = serde_json::from_str(
            r#"{"process":{"tag":"nope"},"pty":{"size":{"cols":80,"rows":24}}}"#,
        )
        .unwrap();
        let err = update(&state, &req).unwrap_err();
        assert_eq!(err.code, ConnectCode::NotFound);
    }

    #[test]
    fn update_non_pty_process_is_internal() {
        let state = AppState::new();
        let (sender, _rx) = broadcast::channel::<exec::PumpEvent>(1);
        state.insert_process(ProcEntry {
            pid: 7,
            tag: None,
            config: crate::msg::process::ProcessConfig::default(),
            sender,
            pty_master: None,
            input: disabled_input(),
            cgroup: None,
            termination: Arc::new(std::sync::Mutex::new(None)),
            terminal: Arc::new(std::sync::Mutex::new(None)),
        });
        let req: UpdateRequest =
            serde_json::from_str(r#"{"process":{"pid":7},"pty":{"size":{"cols":80,"rows":24}}}"#)
                .unwrap();
        let err = update(&state, &req).unwrap_err();
        assert_eq!(err.code, ConnectCode::Internal);
        assert_eq!(
            err.message,
            "error resizing tty: tty not assigned to process"
        );
    }

    #[tokio::test]
    async fn pipe_input_and_close_stdin_reach_the_child() {
        use base64::Engine;

        let state = AppState::new();
        let spawned = exec::spawn(
            "/bin/cat",
            &[],
            std::collections::HashMap::new(),
            "/".into(),
            &current_user(),
            true,
            None,
        )
        .unwrap();
        let (pid, handle, mut events, _completion, _sender, _reaped, _termination) =
            insert_spawned(&state, spawned);
        let data = base64::engine::general_purpose::STANDARD.encode(b"pipe-input\n");
        let req: SendInputRequest = serde_json::from_value(serde_json::json!({
            "process": {"pid": pid},
            "input": {"stdin": data}
        }))
        .unwrap();
        assert_eq!(
            send_input(&state, &req).await.unwrap(),
            serde_json::json!({})
        );

        let close: CloseStdinRequest =
            serde_json::from_value(serde_json::json!({"process": {"pid": pid}})).unwrap();
        assert_eq!(
            close_stdin(&state, &close).await.unwrap(),
            serde_json::json!({})
        );
        // Closing an already-closed pipe is intentionally idempotent.
        close_stdin(&state, &close).await.unwrap();

        let mut stdout = Vec::new();
        loop {
            match tokio::time::timeout(std::time::Duration::from_secs(3), events.recv())
                .await
                .expect("cat did not exit after stdin EOF")
                .expect("event bus closed before End")
            {
                exec::PumpEvent::Data(data) => {
                    if let Some(chunk) = data.stdout {
                        stdout.extend(
                            base64::engine::general_purpose::STANDARD
                                .decode(chunk)
                                .unwrap(),
                        );
                    }
                }
                exec::PumpEvent::End(end) => {
                    assert_eq!(end.exit_code, 0);
                    break;
                }
                exec::PumpEvent::SpawnError(e) => panic!("spawn error: {e}"),
                exec::PumpEvent::DeadlineExceeded => panic!("unexpected deadline"),
            }
        }
        assert_eq!(stdout, b"pipe-input\n");
        state.remove_process(handle);
    }

    #[tokio::test]
    async fn pty_input_uses_master_and_close_stdin_is_rejected() {
        use base64::Engine;

        let state = AppState::new();
        let spawned = exec::spawn_pty(
            "/bin/sh",
            &[
                "-c".into(),
                "read line; printf 'got:%s\\n' \"$line\"; read rest".into(),
            ],
            std::collections::HashMap::new(),
            "/".into(),
            &current_user(),
            (80, 24),
            None,
        )
        .unwrap();
        let (pid, handle, mut events, _completion, _sender, _reaped, _termination) =
            insert_spawned(&state, spawned);
        let req: SendInputRequest = serde_json::from_value(serde_json::json!({
            "process": {"pid": pid},
            "input": {"pty": base64::engine::general_purpose::STANDARD.encode(b"hello\n")}
        }))
        .unwrap();
        send_input(&state, &req).await.unwrap();

        let wrong: SendInputRequest = serde_json::from_value(serde_json::json!({
            "process": {"pid": pid},
            "input": {"stdin": base64::engine::general_purpose::STANDARD.encode(b"x")}
        }))
        .unwrap();
        assert!(send_input(&state, &wrong)
            .await
            .unwrap_err()
            .message
            .contains("tty assigned to process"));
        let close: CloseStdinRequest =
            serde_json::from_value(serde_json::json!({"process": {"pid": pid}})).unwrap();
        let error = close_stdin(&state, &close).await.unwrap_err();
        assert_eq!(error.code, ConnectCode::Unknown);
        assert!(error.message.contains("cannot close stdin for PTY process"));

        let mut output = Vec::new();
        while !String::from_utf8_lossy(&output).contains("got:hello") {
            match tokio::time::timeout(std::time::Duration::from_secs(3), events.recv())
                .await
                .expect("PTY did not consume input")
                .expect("event bus closed before expected output")
            {
                exec::PumpEvent::Data(data) => {
                    if let Some(chunk) = data.pty {
                        output.extend(
                            base64::engine::general_purpose::STANDARD
                                .decode(chunk)
                                .unwrap(),
                        );
                    }
                }
                exec::PumpEvent::End(end) => panic!("PTY exited early: {end:?}"),
                exec::PumpEvent::SpawnError(e) => panic!("spawn error: {e}"),
                exec::PumpEvent::DeadlineExceeded => panic!("unexpected deadline"),
            }
        }
        let eof: SendInputRequest = serde_json::from_value(serde_json::json!({
            "process": {"pid": pid},
            "input": {"pty": base64::engine::general_purpose::STANDARD.encode([0x04])}
        }))
        .unwrap();
        send_input(&state, &eof).await.unwrap();
        loop {
            match tokio::time::timeout(std::time::Duration::from_secs(3), events.recv())
                .await
                .expect("PTY did not exit after Ctrl-D")
                .expect("event bus closed before terminal event")
            {
                exec::PumpEvent::End(_) => break,
                exec::PumpEvent::Data(_) => {}
                event => panic!("unexpected terminal event: {event:?}"),
            }
        }
        state.remove_process(handle);
    }

    #[tokio::test]
    async fn connect_driver_exits_when_response_receiver_disconnects() {
        let (_pub_tx, events) = broadcast::channel::<exec::PumpEvent>(4);
        let (tx, mut rx) = mpsc::channel::<Bytes>(4);
        let driver = tokio::spawn(drive_stream(
            42,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));
        rx.recv().await.expect("start frame");
        drop(rx);
        tokio::time::timeout(std::time::Duration::from_millis(500), driver)
            .await
            .expect("detached Connect driver leaked after client disconnect")
            .unwrap();
    }

    #[tokio::test]
    async fn connect_timeout_ends_only_the_attachment() {
        let state = Arc::new(AppState::new());
        let spawned = exec::spawn(
            "/bin/sh",
            &["-c".into(), "sleep 1".into()],
            std::collections::HashMap::new(),
            "/".into(),
            &current_user(),
            false,
            None,
        )
        .unwrap();
        let (pid, handle, events, completion, sender, reaped, termination) =
            insert_spawned(&state, spawned);
        let supervisor = tokio::spawn(supervise_process(
            state.clone(),
            handle,
            pid,
            sender,
            completion,
            None,
            None,
            reaped,
            termination,
        ));
        let (tx, mut rx) = mpsc::channel::<Bytes>(4);
        let driver = tokio::spawn(drive_stream(
            pid,
            events,
            tx,
            std::time::Duration::from_secs(30),
            Some(std::time::Duration::from_millis(20)),
        ));
        assert_eq!(rx.recv().await.unwrap()[0], 0);
        let terminal = rx.recv().await.expect("Connect timeout frame");
        assert_eq!(terminal[0], connect::END_STREAM_FLAG);
        let payload: serde_json::Value = serde_json::from_slice(&terminal[5..]).unwrap();
        assert_eq!(payload["error"]["code"], "deadline_exceeded");
        assert!(state.find_pid(Some(pid), None).is_some());
        driver.await.unwrap();
        exec::kill_process_group(pid, libc::SIGKILL).unwrap();
        supervisor.await.unwrap();
        assert!(state.find_pid(Some(pid), None).is_none());
    }

    #[tokio::test]
    async fn connect_without_server_deadline_survives_timeout_header_semantics() {
        let state = Arc::new(AppState::new());
        let spawned = exec::spawn(
            "/bin/sh",
            &["-c".into(), "sleep 1".into()],
            std::collections::HashMap::new(),
            "/".into(),
            &current_user(),
            false,
            None,
        )
        .unwrap();
        let (pid, handle, events, completion, sender, reaped, termination) =
            insert_spawned(&state, spawned);
        let supervisor = tokio::spawn(supervise_process(
            state.clone(),
            handle,
            pid,
            sender,
            completion,
            None,
            None,
            reaped,
            termination,
        ));
        let (tx, mut rx) = mpsc::channel::<Bytes>(4);
        let driver = tokio::spawn(drive_stream(
            pid,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));
        assert_eq!(rx.recv().await.unwrap()[0], 0);
        assert!(
            tokio::time::timeout(std::time::Duration::from_millis(50), rx.recv())
                .await
                .is_err()
        );
        let terminal = rx.recv().await.expect("process EndEvent and EndStream");
        assert_eq!(terminal[0], 0);
        driver.await.unwrap();
        supervisor.await.unwrap();
    }

    #[tokio::test]
    async fn closed_output_bus_returns_explicit_error_frame() {
        let (pub_tx, events) = broadcast::channel::<exec::PumpEvent>(4);
        let (tx, mut rx) = mpsc::channel::<Bytes>(4);
        let driver = tokio::spawn(drive_stream(
            42,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));
        assert_eq!(rx.recv().await.unwrap()[0], 0);
        drop(pub_tx);
        let terminal = rx.recv().await.expect("unexpected closed-bus frame");
        assert_eq!(terminal[0], connect::END_STREAM_FLAG);
        let payload: serde_json::Value = serde_json::from_slice(&terminal[5..]).unwrap();
        assert_eq!(payload["error"]["code"], "internal");
        assert!(rx.recv().await.is_none());
        driver.await.unwrap();
    }

    #[tokio::test]
    async fn process_supervisor_cleans_up_after_start_response_disconnects() {
        let state = Arc::new(AppState::new());
        let spawned = exec::spawn(
            "/bin/sh",
            &["-c".into(), "sleep 1".into()],
            std::collections::HashMap::new(),
            "/".into(),
            &current_user(),
            false,
            None,
        )
        .unwrap();
        let (pid, handle, events, completion, sender, reaped, termination) =
            insert_spawned(&state, spawned);
        let supervisor = tokio::spawn(supervise_process(
            state.clone(),
            handle,
            pid,
            sender,
            completion,
            None,
            None,
            reaped,
            termination,
        ));
        let (tx, mut rx) = mpsc::channel::<Bytes>(4);
        let driver = tokio::spawn(drive_stream(
            pid,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));
        rx.recv().await.expect("start frame");
        drop(rx);
        tokio::time::timeout(std::time::Duration::from_millis(500), driver)
            .await
            .expect("Start response task did not exit after disconnect")
            .unwrap();
        assert!(
            state.find_pid(Some(pid), None).is_some(),
            "disconnect must not remove a still-running process"
        );
        tokio::time::timeout(std::time::Duration::from_secs(3), supervisor)
            .await
            .expect("supervisor did not reap the disconnected process")
            .unwrap();
        assert!(state.find_pid(Some(pid), None).is_none());
    }

    #[tokio::test]
    async fn process_supervisor_deadline_is_delivered_without_http_backpressure() {
        let state = Arc::new(AppState::new());
        let spawned = exec::spawn(
            "/bin/sh",
            &["-c".into(), "sleep 30".into()],
            std::collections::HashMap::new(),
            "/".into(),
            &current_user(),
            false,
            None,
        )
        .unwrap();
        let (pid, handle, events, completion, sender, reaped, termination) =
            insert_spawned(&state, spawned);
        let supervisor = tokio::spawn(supervise_process(
            state.clone(),
            handle,
            pid,
            sender,
            completion,
            Some(std::time::Duration::from_millis(20)),
            None,
            reaped,
            termination,
        ));
        let (tx, mut rx) = mpsc::channel::<Bytes>(4);
        let driver = tokio::spawn(drive_stream(
            pid,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));

        let start = rx.recv().await.expect("start frame");
        assert_eq!(start[0], 0);
        let terminal = rx.recv().await.expect("deadline EndEvent and EndStream");
        assert_eq!(terminal[0], 0);
        let size = u32::from_be_bytes(terminal[1..5].try_into().unwrap()) as usize;
        let end: serde_json::Value = serde_json::from_slice(&terminal[5..5 + size]).unwrap();
        assert_eq!(end["event"]["end"]["signal"], libc::SIGKILL);
        assert_eq!(end["event"]["end"]["killedBy"], "timeout");
        let trailer = &terminal[5 + size..];
        assert_eq!(trailer[0], connect::END_STREAM_FLAG);
        let payload: serde_json::Value = serde_json::from_slice(&trailer[5..]).unwrap();
        assert_eq!(payload["error"]["code"], "deadline_exceeded");
        assert!(state.find_pid(Some(pid), None).is_none());
        driver.await.unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(3), supervisor)
            .await
            .expect("deadline process was not reaped")
            .unwrap();
    }

    #[tokio::test]
    async fn deadline_does_not_misclassify_child_reaped_during_output_drain() {
        let state = Arc::new(AppState::new());
        let spawned = exec::spawn(
            "/bin/sh",
            &["-c".into(), "sleep 1 & exit 0".into()],
            std::collections::HashMap::new(),
            "/".into(),
            &current_user(),
            false,
            None,
        )
        .unwrap();
        let (pid, handle, events, completion, sender, reaped, termination) =
            insert_spawned(&state, spawned);
        let supervisor = tokio::spawn(supervise_process(
            state.clone(),
            handle,
            pid,
            sender,
            completion,
            Some(std::time::Duration::from_millis(100)),
            None,
            reaped,
            termination,
        ));
        let (tx, mut rx) = mpsc::channel::<Bytes>(4);
        let driver = tokio::spawn(drive_stream(
            pid,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));
        assert_eq!(rx.recv().await.unwrap()[0], 0);
        let terminal = tokio::time::timeout(std::time::Duration::from_secs(2), rx.recv())
            .await
            .expect("child terminal event did not arrive")
            .expect("response closed before terminal event");
        assert_eq!(
            terminal[0], 0,
            "child was incorrectly classified as timed out"
        );
        let payload_size =
            u32::from_be_bytes([terminal[1], terminal[2], terminal[3], terminal[4]]) as usize;
        let payload: serde_json::Value =
            serde_json::from_slice(&terminal[5..5 + payload_size]).unwrap();
        assert_eq!(payload["event"]["end"]["status"], "exit status 0");
        let trailer_offset = 5 + payload_size;
        assert_eq!(terminal[trailer_offset], connect::END_STREAM_FLAG);
        assert!(
            rx.recv().await.is_none(),
            "response remained open after EndStream"
        );
        driver.await.unwrap();
        supervisor.await.unwrap();
        assert!(state.find_pid(Some(pid), None).is_none());
    }

    #[tokio::test]
    async fn unread_full_response_does_not_block_deadline_or_reaping() {
        let state = Arc::new(AppState::new());
        let spawned = exec::spawn(
            "/bin/sh",
            &["-c".into(), "while :; do printf 1234567890; done".into()],
            std::collections::HashMap::new(),
            "/".into(),
            &current_user(),
            false,
            None,
        )
        .unwrap();
        let (pid, handle, events, completion, sender, reaped, termination) =
            insert_spawned(&state, spawned);
        let supervisor = tokio::spawn(supervise_process(
            state.clone(),
            handle,
            pid,
            sender,
            completion,
            Some(std::time::Duration::from_millis(50)),
            None,
            reaped,
            termination,
        ));
        // No receiver read occurs until after the independent supervisor has
        // enforced the deadline and reaped the child.
        let (tx, mut rx) = mpsc::channel::<Bytes>(RESPONSE_QUEUE_CAPACITY);
        let driver = tokio::spawn(drive_stream(
            pid,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));
        tokio::time::timeout(std::time::Duration::from_secs(3), supervisor)
            .await
            .expect("full HTTP response queue blocked deadline/reaping")
            .unwrap();
        driver.await.unwrap();
        assert!(state.find_pid(Some(pid), None).is_none());
        let mut frames = Vec::new();
        while let Some(frame) = rx.recv().await {
            frames.push(frame);
        }
        assert_eq!(
            frames.first().map(|f| f[0]),
            Some(0),
            "queued Start frame was lost"
        );
        let bytes: Vec<u8> = frames.into_iter().flatten().collect();
        let mut remaining = bytes.as_slice();
        let mut last = None;
        while !remaining.is_empty() {
            assert!(remaining.len() >= 5);
            let length = u32::from_be_bytes(remaining[1..5].try_into().unwrap()) as usize;
            let payload: serde_json::Value =
                serde_json::from_slice(&remaining[5..5 + length]).unwrap();
            last = Some((remaining[0], payload));
            remaining = &remaining[5 + length..];
        }
        let (flags, payload) = last.expect("missing terminal envelope");
        assert_eq!(flags, connect::END_STREAM_FLAG);
        assert!(matches!(
            payload["error"]["code"].as_str(),
            Some("resource_exhausted" | "deadline_exceeded")
        ));
    }

    #[tokio::test]
    async fn backpressure_closes_only_the_response_task() {
        let state = Arc::new(AppState::new());
        let (pub_tx, events) = broadcast::channel::<exec::PumpEvent>(4);
        let _handle = state.insert_process(ProcEntry {
            pid: 42,
            tag: None,
            config: crate::msg::process::ProcessConfig::default(),
            sender: pub_tx.clone(),
            pty_master: None,
            input: disabled_input(),
            cgroup: None,
            termination: Arc::new(std::sync::Mutex::new(None)),
            terminal: Arc::new(std::sync::Mutex::new(None)),
        });
        // Start consumes the first slot; the second is reserved for the
        // resource_exhausted EndStream frame when Data cannot be queued.
        let (tx, mut rx) = mpsc::channel::<Bytes>(2);
        let driver = tokio::spawn(drive_stream(
            42,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));
        tokio::task::yield_now().await;
        assert!(pub_tx
            .send(exec::PumpEvent::Data(crate::msg::process::DataEvent {
                stdout: Some("eA==".into()),
                ..Default::default()
            }))
            .is_ok());
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;

        assert!(rx.recv().await.is_some(), "Start frame missing");
        let terminal = rx.recv().await.expect("backpressure error missing");
        assert_eq!(terminal[0], connect::END_STREAM_FLAG);
        let payload: serde_json::Value = serde_json::from_slice(&terminal[5..]).unwrap();
        assert_eq!(payload["error"]["code"], "resource_exhausted");
        assert!(
            tokio::time::timeout(std::time::Duration::from_millis(200), rx.recv())
                .await
                .expect("response producer remained open after backpressure")
                .is_none()
        );
        assert!(
            state.find_pid(Some(42), None).is_some(),
            "closing the response must not reap a still-running Start process"
        );

        driver.await.unwrap();
        assert!(state.find_pid(Some(42), None).is_some());
    }

    #[tokio::test]
    async fn end_event_and_end_stream_share_one_queue_slot() {
        let (pub_tx, events) = broadcast::channel::<exec::PumpEvent>(4);
        let (tx, mut rx) = mpsc::channel::<Bytes>(2);
        let driver = tokio::spawn(drive_stream(
            42,
            events,
            tx,
            std::time::Duration::from_secs(30),
            None,
        ));
        tokio::task::yield_now().await;
        assert!(pub_tx
            .send(exec::PumpEvent::End(crate::msg::process::EndEvent {
                exit_code: 0,
                exited: true,
                status: "exit status 0".into(),
                error: None,
                signal: None,
                oom_killed: None,
                killed_by: None,
            }))
            .is_ok());
        driver.await.unwrap();

        let start = rx.recv().await.expect("Start queue item");
        assert_eq!(start[0], 0);
        let terminal = rx.recv().await.expect("combined terminal queue item");
        let event_size =
            u32::from_be_bytes([terminal[1], terminal[2], terminal[3], terminal[4]]) as usize;
        let trailer_offset = 5 + event_size;
        assert_eq!(terminal[0], 0);
        assert_eq!(terminal[trailer_offset], connect::END_STREAM_FLAG);
        assert_eq!(&terminal[trailer_offset + 5..], b"{}");
        assert!(rx.recv().await.is_none());
    }

    #[tokio::test]
    async fn stream_input_requires_start_and_reuses_selected_writer() {
        let state = AppState::new();
        let (sender, _events) = broadcast::channel::<exec::PumpEvent>(1);
        state.insert_process(ProcEntry {
            pid: 7,
            tag: Some("shell".into()),
            config: crate::msg::process::ProcessConfig::default(),
            sender,
            pty_master: None,
            input: disabled_input(),
            cgroup: None,
            termination: Arc::new(std::sync::Mutex::new(None)),
            terminal: Arc::new(std::sync::Mutex::new(None)),
        });
        let mut selected = None;
        let data: StreamInputRequest = serde_json::from_value(serde_json::json!({
            "data": {"input": {"stdin": "eA=="}}
        }))
        .unwrap();
        assert_eq!(
            stream_input_event(&state, &mut selected, data.clone())
                .await
                .unwrap_err()
                .code,
            ConnectCode::InvalidArgument
        );
        let start: StreamInputRequest = serde_json::from_value(serde_json::json!({
            "start": {"process": {"tag": "shell"}}
        }))
        .unwrap();
        stream_input_event(&state, &mut selected, start)
            .await
            .unwrap();
        assert!(selected.is_some());
        assert!(stream_input_event(&state, &mut selected, data)
            .await
            .unwrap_err()
            .message
            .contains("stdin not enabled or closed"));
        let keepalive: StreamInputRequest =
            serde_json::from_value(serde_json::json!({"keepalive": {}})).unwrap();
        stream_input_event(&state, &mut selected, keepalive)
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn input_oneof_validation_reports_unimplemented() {
        let state = AppState::new();
        let (sender, _events) = broadcast::channel::<exec::PumpEvent>(1);
        let pid = state.insert_process(ProcEntry {
            pid: 8,
            tag: None,
            config: crate::msg::process::ProcessConfig::default(),
            sender,
            pty_master: None,
            input: disabled_input(),
            cgroup: None,
            termination: Arc::new(std::sync::Mutex::new(None)),
            terminal: Arc::new(std::sync::Mutex::new(None)),
        });

        let send = SendInputRequest {
            process: ProcessSelector {
                pid: Some(8),
                tag: None,
            },
            input: ProcessInput::default(),
        };
        let error = send_input(&state, &send).await.unwrap_err();
        assert_eq!(error.code, ConnectCode::Unimplemented);
        assert_eq!(error.message, "invalid input type <nil>");

        let mut selected = None;
        let malformed = StreamInputRequest::default();
        let error = stream_input_event(&state, &mut selected, malformed)
            .await
            .unwrap_err();
        assert_eq!(error.code, ConnectCode::Unimplemented);
        assert_eq!(error.message, "invalid event type <nil>");
        state.remove_process(pid);
    }

    #[tokio::test]
    async fn drive_stream_lagged_cuts_off_slow_subscriber() {
        // Capacity-1 ring: publishing two events before the driver reads any
        // overflows the ring, so its first recv() reports Lagged instead of
        // delivering the overwritten event.
        let (pub_tx, events) = broadcast::channel::<exec::PumpEvent>(1);
        let data = |s: &str| {
            exec::PumpEvent::Data(crate::msg::process::DataEvent {
                stdout: Some(s.into()),
                ..Default::default()
            })
        };
        assert!(pub_tx.send(data("a")).is_ok());
        assert!(pub_tx.send(data("b")).is_ok());

        let (tx, mut rx) = mpsc::channel::<Bytes>(16);
        let driver = tokio::spawn(async move {
            drive_stream(42, events, tx, std::time::Duration::from_secs(30), None).await;
        });

        let mut frames = Vec::new();
        while let Some(f) = rx.recv().await {
            frames.push(f);
        }
        driver.await.unwrap();

        // Start, then exactly one terminal EndStream error frame — no Data,
        // no End event, no end_stream_ok. The lagging subscriber is cut off
        // and no process-lifecycle work is retained in this response task.
        assert_eq!(frames.len(), 2);
        let start: serde_json::Value = serde_json::from_slice(&frames[0][5..]).unwrap();
        assert_eq!(start["event"]["start"]["pid"], 42);
        assert_eq!(frames[1][0], connect::END_STREAM_FLAG);
        let err: serde_json::Value = serde_json::from_slice(&frames[1][5..]).unwrap();
        assert_eq!(err["error"]["code"], "resource_exhausted");
    }
}

use crate::connect::{decode_frame, encode_end_stream, encode_stream_message};
use crate::defaults::{self, Defaults};
use crate::process::connect_timeout;
use crate::processes::{self, ProcessRecord};
use crate::termination::{self, CgroupMemoryMonitor, TerminationInfo};
use axum::{
    body::Body,
    http::{header, HeaderMap, StatusCode},
    response::Response,
};
use base64::{engine::general_purpose::STANDARD, Engine as _};
use portable_pty::{native_pty_system, CommandBuilder, MasterPty, PtySize};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashMap,
    io::{self, Read, Write},
    sync::atomic::{AtomicBool, Ordering},
    sync::{Arc, Mutex, OnceLock},
    time::Duration,
};
use tokio::{
    sync::{broadcast, mpsc},
    task, time,
};
use tokio_stream::{wrappers::ReceiverStream, StreamExt};

static PTY_SESSIONS: OnceLock<Mutex<HashMap<u32, Arc<PtySession>>>> = OnceLock::new();

fn sessions() -> &'static Mutex<HashMap<u32, Arc<PtySession>>> {
    PTY_SESSIONS.get_or_init(|| Mutex::new(HashMap::new()))
}

struct PtySession {
    pid: u32,
    master: Mutex<Box<dyn MasterPty + Send>>,
    writer: Mutex<Box<dyn Write + Send>>,
    events: broadcast::Sender<PtyEvent>,
    history: Mutex<Vec<PtyEvent>>,
    oom_monitor: CgroupMemoryMonitor,
    timed_out: AtomicBool,
    alive: AtomicBool,
}

#[derive(Clone, Debug)]
enum PtyEvent {
    Data(Vec<u8>),
    End {
        exit_code: Option<i32>,
        exited: bool,
        status: String,
        error: Option<String>,
        termination: TerminationInfo,
    },
}

#[derive(Debug, Deserialize)]
struct PtyStartRequest {
    process: PtyProcessConfig,
    pty: PtyConfig,
    #[serde(default)]
    tag: Option<String>,
}

#[derive(Debug, Deserialize)]
struct PtyProcessConfig {
    cmd: String,
    #[serde(default)]
    args: Vec<String>,
    #[serde(default)]
    envs: HashMap<String, String>,
    #[serde(default)]
    cwd: Option<String>,
}

#[derive(Debug, Deserialize)]
struct PtySelectorRequest {
    process: PtySelector,
}

#[derive(Debug, Deserialize)]
struct PtySelector {
    pid: u32,
}

#[derive(Debug, Deserialize)]
struct PtyConfig {
    size: PtySizeWire,
}

#[derive(Debug, Deserialize)]
struct PtySizeWire {
    rows: u16,
    cols: u16,
}

#[derive(Debug, Deserialize)]
struct PtySignalRequest {
    process: PtySelector,
    signal: String,
}

#[derive(Debug, Deserialize)]
struct PtyInputRequest {
    process: PtySelector,
    input: PtyInput,
}

#[derive(Debug, Deserialize)]
struct PtyInput {
    #[serde(default)]
    stdin: Option<String>,
    #[serde(default)]
    pty: Option<String>,
}

#[derive(Debug, Deserialize)]
struct PtyUpdateRequest {
    process: PtySelector,
    pty: PtyConfig,
}

#[derive(Debug, Serialize)]
struct PtyResponse {
    event: PtyEventWire,
}

#[derive(Debug, Serialize)]
struct PtyEventWire {
    #[serde(skip_serializing_if = "Option::is_none")]
    start: Option<PtyStartEvent>,
    #[serde(skip_serializing_if = "Option::is_none")]
    data: Option<PtyDataEvent>,
    #[serde(skip_serializing_if = "Option::is_none")]
    end: Option<PtyEndEvent>,
}

#[derive(Debug, Serialize)]
struct PtyStartEvent {
    pid: u32,
}

#[derive(Debug, Serialize)]
struct PtyDataEvent {
    pty: String,
}

#[derive(Debug, Serialize)]
struct PtyEndEvent {
    #[serde(rename = "exitCode", skip_serializing_if = "Option::is_none")]
    exit_code: Option<i32>,
    exited: bool,
    status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    termination: Option<TerminationInfo>,
}

pub async fn start(headers: HeaderMap, body: bytes::Bytes) -> Response {
    let request = match decode_request::<PtyStartRequest>(&body) {
        Ok(request) => request,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error),
    };
    let size = pty_size(request.pty.size);
    let defaults = defaults::snapshot();
    let record = ProcessRecord {
        cmd: request.process.cmd.clone(),
        args: request.process.args.clone(),
        envs: defaults.merged_env_vars(&request.process.envs),
        cwd: request.process.cwd.clone().or_else(|| defaults.workdir()),
        tag: request.tag.clone(),
    };
    let command = match build_command(request.process, &defaults) {
        Ok(command) => command,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
    };
    let timeout = match connect_timeout(&headers) {
        Ok(timeout) => timeout,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error),
    };
    let session = match create_session(command, size, timeout, record) {
        Ok(session) => session,
        Err(error) => return error_response(StatusCode::INTERNAL_SERVER_ERROR, error.to_string()),
    };
    stream_session(session).await
}

pub async fn connect(body: bytes::Bytes) -> Response {
    let request = match decode_request::<PtySelectorRequest>(&body) {
        Ok(request) => request,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error),
    };
    let session = match find_session(request.process.pid) {
        Some(session) => session,
        None => return not_found_response(),
    };
    stream_session(session).await
}

pub async fn send_signal(body: bytes::Bytes) -> Response {
    let request = match serde_json::from_slice::<PtySignalRequest>(&body) {
        Ok(request) => request,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
    };
    if request.signal != "SIGNAL_SIGKILL" {
        return error_response(
            StatusCode::BAD_REQUEST,
            "only SIGNAL_SIGKILL is supported".to_owned(),
        );
    }
    let session = match find_session(request.process.pid) {
        Some(session) => session,
        None => return not_found_response(),
    };
    #[cfg(unix)]
    {
        use nix::{
            sys::signal::{kill, Signal},
            unistd::Pid,
        };
        if let Err(error) = kill(Pid::from_raw(-(session.pid as i32)), Signal::SIGKILL) {
            return error_response(StatusCode::INTERNAL_SERVER_ERROR, error.to_string());
        }
    }
    #[cfg(not(unix))]
    {
        let _ = session;
        return error_response(
            StatusCode::NOT_IMPLEMENTED,
            "signals are only supported on Unix".to_owned(),
        );
    }
    empty_response()
}

pub async fn send_input(body: bytes::Bytes) -> Response {
    let request = match serde_json::from_slice::<PtyInputRequest>(&body) {
        Ok(request) => request,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
    };
    let stdin_arm = request.input.stdin.as_deref();
    let pty_arm = request.input.pty.as_deref();
    if stdin_arm.is_some() as u8 + pty_arm.is_some() as u8 != 1 {
        return error_response(
            StatusCode::NOT_IMPLEMENTED,
            "SendInput input must set exactly one arm".to_owned(),
        );
    }
    let encoded = stdin_arm.or(pty_arm).expect("exactly one arm is set");
    let input = match STANDARD.decode(encoded) {
        Ok(input) => input,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
    };
    if stdin_arm.is_some() {
        return match crate::processes::write_stdin(request.process.pid, &input).await {
            Ok(true) => empty_response(),
            Ok(false) => error_response(
                StatusCode::NOT_IMPLEMENTED,
                format!("no stdin for process {}", request.process.pid),
            ),
            Err(error) => error_response(StatusCode::INTERNAL_SERVER_ERROR, error.to_string()),
        };
    }
    match write_pty_input(request.process.pid, &input) {
        Ok(true) => empty_response(),
        Ok(false) => not_found_response(),
        Err(message) => error_response(StatusCode::INTERNAL_SERVER_ERROR, message),
    }
}

/// Write raw bytes to an existing PTY session. `Ok(false)` means no such
/// session, so callers can map it to their own not-found handling.
pub(crate) fn write_pty_input(pid: u32, input: &[u8]) -> Result<bool, String> {
    let Some(session) = find_session(pid) else {
        return Ok(false);
    };
    let mut writer = match session.writer.lock() {
        Ok(writer) => writer,
        Err(_) => return Err("PTY writer lock poisoned".to_owned()),
    };
    writer
        .write_all(input)
        .and_then(|_| writer.flush())
        .map_err(|error| error.to_string())?;
    Ok(true)
}

pub async fn update(body: bytes::Bytes) -> Response {
    let request = match serde_json::from_slice::<PtyUpdateRequest>(&body) {
        Ok(request) => request,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
    };
    let session = match find_session(request.process.pid) {
        Some(session) => session,
        None => return not_found_response(),
    };
    let master = match session.master.lock() {
        Ok(master) => master,
        Err(_) => {
            return error_response(
                StatusCode::INTERNAL_SERVER_ERROR,
                "PTY master lock poisoned".to_owned(),
            )
        }
    };
    if let Err(error) = master.resize(pty_size(request.pty.size)) {
        return error_response(StatusCode::INTERNAL_SERVER_ERROR, error.to_string());
    }
    empty_response()
}

fn create_session(
    command: CommandBuilder,
    size: PtySize,
    timeout: Option<Duration>,
    record: ProcessRecord,
) -> io::Result<Arc<PtySession>> {
    let pair = native_pty_system()
        .openpty(size)
        .map_err(|error| io::Error::other(error.to_string()))?;
    let child = pair
        .slave
        .spawn_command(command)
        .map_err(|error| io::Error::other(error.to_string()))?;
    let pid = child
        .process_id()
        .ok_or_else(|| io::Error::other("PTY child did not expose a process ID"))?;
    let reader = pair
        .master
        .try_clone_reader()
        .map_err(|error| io::Error::other(error.to_string()))?;
    let writer = pair
        .master
        .take_writer()
        .map_err(|error| io::Error::other(error.to_string()))?;
    let (events, _) = broadcast::channel(64);
    let session = Arc::new(PtySession {
        pid,
        master: Mutex::new(pair.master),
        writer: Mutex::new(writer),
        events,
        history: Mutex::new(Vec::new()),
        oom_monitor: CgroupMemoryMonitor::start(),
        timed_out: AtomicBool::new(false),
        alive: AtomicBool::new(true),
    });
    sessions()
        .lock()
        .expect("PTY session lock poisoned")
        .insert(pid, Arc::clone(&session));
    processes::register(pid, record);
    spawn_pty_worker(session.clone(), reader, child);
    schedule_timeout(session.clone(), timeout);
    Ok(session)
}

fn schedule_timeout(session: Arc<PtySession>, timeout: Option<Duration>) {
    let Some(timeout) = timeout else {
        return;
    };
    task::spawn(async move {
        time::sleep(timeout).await;
        if !session.alive.load(Ordering::Acquire) {
            return;
        }
        session.timed_out.store(true, Ordering::Release);
        #[cfg(unix)]
        {
            // The PTY child is a session/process-group leader, so -pid reaps
            // the whole tree, not just the shell.
            let _ = nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(-(session.pid as i32)),
                nix::sys::signal::Signal::SIGKILL,
            );
        }
    });
}

fn spawn_pty_worker(
    session: Arc<PtySession>,
    mut reader: Box<dyn Read + Send>,
    child: Box<dyn portable_pty::Child + Send>,
) {
    let pid = session.pid;
    task::spawn_blocking(move || {
        let mut buffer = vec![0u8; 8192];
        loop {
            match reader.read(&mut buffer) {
                Ok(0) | Err(_) => break,
                Ok(size) => record_event(&session, PtyEvent::Data(buffer[..size].to_vec())),
            }
        }
        let oom_killed = session.oom_monitor.was_oom_killed();
        let status = wait_for_pty_child(pid, child);
        session.alive.store(false, Ordering::Release);
        let event = match status {
            Ok(status) => pty_end_event(
                status,
                oom_killed,
                session.timed_out.load(Ordering::Acquire),
            ),
            Err(error) => PtyEvent::End {
                exit_code: None,
                exited: false,
                status: error.to_string(),
                error: Some(error.to_string()),
                termination: TerminationInfo::unknown(),
            },
        };
        record_event(&session, event);
        sessions()
            .lock()
            .expect("PTY session lock poisoned")
            .remove(&pid);
        processes::unregister(pid);
    });
}

#[cfg(unix)]
fn wait_for_pty_child(
    pid: u32,
    _child: Box<dyn portable_pty::Child + Send>,
) -> nix::Result<nix::sys::wait::WaitStatus> {
    nix::sys::wait::waitpid(nix::unistd::Pid::from_raw(pid as i32), None)
}

#[cfg(not(unix))]
fn wait_for_pty_child(
    _pid: u32,
    child: Box<dyn portable_pty::Child + Send>,
) -> io::Result<portable_pty::ExitStatus> {
    let mut child = child;
    child
        .wait()
        .map_err(|error| io::Error::other(error.to_string()))
}

#[cfg(unix)]
fn pty_end_event(
    status: nix::sys::wait::WaitStatus,
    oom_killed: bool,
    timed_out: bool,
) -> PtyEvent {
    use nix::sys::wait::WaitStatus;

    match status {
        WaitStatus::Exited(_, exit_code) => {
            // Upstream envd (Go) mirrors a non-zero exit into the end-event
            // `error` as "exit status N" while keeping `exited: true`.
            let text = format!("exit status {exit_code}");
            if timed_out {
                PtyEvent::End {
                    exit_code: None,
                    exited: false,
                    status: text,
                    error: Some("process timed out".to_owned()),
                    termination: TerminationInfo::timeout(),
                }
            } else {
                PtyEvent::End {
                    exit_code: Some(exit_code),
                    exited: true,
                    status: text.clone(),
                    error: (exit_code != 0).then_some(text),
                    termination: TerminationInfo::exited(),
                }
            }
        }
        WaitStatus::Signaled(_, signal, core_dumped) => {
            // Match the upstream Go envd contract: a signal-killed process
            // reports exitCode -1 (never 128+signal) with `exited: false` and
            // a "signal: <name>" status string.
            let exit_code = -1;
            let text = format!("signal: {}", termination::legacy_signal_name(signal as i32));
            PtyEvent::End {
                exit_code: (!timed_out).then_some(exit_code),
                exited: false,
                status: text.clone(),
                error: Some(if timed_out {
                    "process timed out".to_owned()
                } else {
                    text
                }),
                termination: if timed_out {
                    TerminationInfo::timeout()
                } else {
                    TerminationInfo::signal(signal as i32, core_dumped, oom_killed)
                },
            }
        }
        other => {
            let message = format!("unexpected child status {other:?}");
            PtyEvent::End {
                exit_code: None,
                exited: false,
                status: message.clone(),
                error: Some(message),
                termination: TerminationInfo::unknown(),
            }
        }
    }
}

#[cfg(not(unix))]
fn pty_end_event(status: portable_pty::ExitStatus, _oom_killed: bool, timed_out: bool) -> PtyEvent {
    let exit_code = status.exit_code() as i32;
    let text = format!("exit status {exit_code}");
    PtyEvent::End {
        exit_code: (!timed_out).then_some(exit_code),
        exited: !timed_out,
        status: text.clone(),
        error: if timed_out {
            Some("process timed out".to_owned())
        } else {
            (exit_code != 0).then_some(text)
        },
        termination: if timed_out {
            TerminationInfo::timeout()
        } else {
            TerminationInfo::exited()
        },
    }
}

fn record_event(session: &PtySession, event: PtyEvent) {
    let mut history = session.history.lock().expect("PTY history lock poisoned");
    if history.len() >= 256 {
        history.remove(0);
    }
    history.push(event.clone());
    let _ = session.events.send(event);
}

async fn stream_session(session: Arc<PtySession>) -> Response {
    let (mut events, history) = {
        let history = session
            .history
            .lock()
            .expect("PTY history lock poisoned")
            .clone();
        (session.events.subscribe(), history)
    };
    let (sender, receiver) = mpsc::channel::<Result<bytes::Bytes, io::Error>>(32);
    send_frame(
        &sender,
        PtyResponse {
            event: PtyEventWire {
                start: Some(PtyStartEvent { pid: session.pid }),
                data: None,
                end: None,
            },
        },
    )
    .await;
    tokio::spawn(async move {
        for event in history {
            if !send_pty_event(&sender, event).await {
                return;
            }
        }
        loop {
            match events.recv().await {
                Ok(event) => {
                    if !send_pty_event(&sender, event.clone()).await {
                        return;
                    }
                    if matches!(event, PtyEvent::End { .. }) {
                        return;
                    }
                }
                Err(broadcast::error::RecvError::Lagged(_)) => continue,
                Err(broadcast::error::RecvError::Closed) => return,
            }
        }
    });
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/connect+json")
        .body(Body::from_stream(
            ReceiverStream::new(receiver).map(|item| item),
        ))
        .expect("valid PTY stream response")
}

async fn send_pty_event(
    sender: &mpsc::Sender<Result<bytes::Bytes, io::Error>>,
    event: PtyEvent,
) -> bool {
    match event {
        PtyEvent::Data(data) => {
            send_frame(
                sender,
                PtyResponse {
                    event: PtyEventWire {
                        start: None,
                        data: Some(PtyDataEvent {
                            pty: STANDARD.encode(data),
                        }),
                        end: None,
                    },
                },
            )
            .await;
        }
        PtyEvent::End {
            exit_code,
            exited,
            status,
            error,
            termination,
        } => {
            send_frame(
                sender,
                PtyResponse {
                    event: PtyEventWire {
                        start: None,
                        data: None,
                        end: Some(PtyEndEvent {
                            exit_code,
                            exited,
                            status,
                            error,
                            termination: Some(termination),
                        }),
                    },
                },
            )
            .await;
            return sender
                .send(Ok(bytes::Bytes::from(encode_end_stream(None))))
                .await
                .is_ok();
        }
    }
    true
}

fn build_command(process: PtyProcessConfig, defaults: &Defaults) -> io::Result<CommandBuilder> {
    if process.cmd.is_empty() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "PTY command is empty",
        ));
    }
    let envs = defaults.merged_env_vars(&process.envs);
    let cwd = process.cwd.or_else(|| defaults.workdir());
    let mut command = CommandBuilder::new(process.cmd);
    command.args(process.args);
    for (key, value) in envs {
        command.env(key, value);
    }
    if let Some(cwd) = cwd {
        command.cwd(cwd);
    }
    Ok(command)
}

fn pty_size(size: PtySizeWire) -> PtySize {
    PtySize {
        rows: size.rows.max(1),
        cols: size.cols.max(1),
        pixel_width: 0,
        pixel_height: 0,
    }
}

fn find_session(pid: u32) -> Option<Arc<PtySession>> {
    sessions()
        .lock()
        .expect("PTY session lock poisoned")
        .get(&pid)
        .cloned()
}

fn decode_request<T: for<'de> Deserialize<'de>>(body: &[u8]) -> Result<T, String> {
    let (flags, payload) = decode_frame(body).map_err(|error| error.to_string())?;
    if flags != 0 {
        return Err("PTY request must be a regular Connect frame".to_owned());
    }
    serde_json::from_slice(&payload).map_err(|error| error.to_string())
}

async fn send_frame<T: Serialize>(
    sender: &mpsc::Sender<Result<bytes::Bytes, io::Error>>,
    value: T,
) {
    if let Ok(payload) = serde_json::to_vec(&value) {
        let _ = sender
            .send(Ok(bytes::Bytes::from(encode_stream_message(&payload))))
            .await;
    }
}

fn empty_response() -> Response {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from("{}"))
        .expect("valid empty response")
}

fn not_found_response() -> Response {
    error_response_with_code(StatusCode::NOT_FOUND, "not_found", "PTY process not found")
}

fn error_response(status: StatusCode, message: String) -> Response {
    error_response_with_code(status, "pty_error", &message)
}

fn error_response_with_code(status: StatusCode, code: &str, message: &str) -> Response {
    let payload = serde_json::json!({"code": code, "message": message});
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(payload.to_string()))
        .expect("valid PTY error response")
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn collect_stream(response: Response) -> Vec<serde_json::Value> {
        let mut stream = response.into_body().into_data_stream();
        let mut frames = Vec::new();
        while let Some(Ok(chunk)) = stream.next().await {
            let (flags, payload) = decode_frame(&chunk).unwrap();
            if flags == 0 {
                frames.push(serde_json::from_slice(&payload).unwrap());
            }
        }
        frames
    }

    #[cfg_attr(not(target_os = "linux"), ignore)]
    #[tokio::test]
    async fn start_emits_pty_data_and_end() {
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/bin/bash","args":["-c","printf hello"]},"pty":{"size":{"rows":24,"cols":80}}}"#,
        ));
        let frames = collect_stream(start(HeaderMap::new(), body).await).await;
        assert!(
            frames.first().unwrap()["event"]["start"]["pid"]
                .as_u64()
                .unwrap()
                > 0
        );
        let output = frames
            .iter()
            .filter_map(|frame| frame["event"]["data"]["pty"].as_str())
            .map(|value| String::from_utf8(STANDARD.decode(value).unwrap()).unwrap())
            .collect::<String>();
        assert!(output.contains("hello"));
        assert!(frames.last().unwrap()["event"]["end"].is_object());
    }

    #[cfg_attr(not(target_os = "linux"), ignore)]
    #[tokio::test]
    async fn input_update_and_signal_operate_on_session() {
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/bin/bash","args":["-i"]},"pty":{"size":{"rows":24,"cols":80}}}"#,
        ));
        let response = start(HeaderMap::new(), body).await;
        let mut stream = response.into_body().into_data_stream();
        let first = stream.next().await.unwrap().unwrap();
        let (_, payload) = decode_frame(&first).unwrap();
        let pid = serde_json::from_slice::<serde_json::Value>(&payload).unwrap()["event"]["start"]
            ["pid"]
            .as_u64()
            .unwrap() as u32;

        let input = serde_json::json!({"process":{"pid":pid},"input":{"pty":STANDARD.encode(b"echo ready\n")}});
        assert_eq!(
            send_input(bytes::Bytes::from(input.to_string()))
                .await
                .status(),
            StatusCode::OK
        );
        let resize =
            serde_json::json!({"process":{"pid":pid},"pty":{"size":{"rows":40,"cols":120}}});
        assert_eq!(
            update(bytes::Bytes::from(resize.to_string()))
                .await
                .status(),
            StatusCode::OK
        );
        let signal = serde_json::json!({"process":{"pid":pid},"signal":"SIGNAL_SIGKILL"});
        assert_eq!(
            send_signal(bytes::Bytes::from(signal.to_string()))
                .await
                .status(),
            StatusCode::OK
        );
        let mut end = None;
        while let Some(Ok(chunk)) = stream.next().await {
            let (flags, payload) = decode_frame(&chunk).unwrap();
            if flags == 0 {
                let frame: serde_json::Value = serde_json::from_slice(&payload).unwrap();
                if frame["event"]["end"].is_object() {
                    end = Some(frame);
                    break;
                }
            }
        }
        let end = end.unwrap()["event"]["end"].clone();
        assert_eq!(end["exitCode"], -1);
        assert_eq!(end["exited"], false);
        assert_eq!(end["status"], "signal: killed");
        assert_eq!(end["error"], "signal: killed");
        assert_eq!(end["termination"]["reason"], "signal");
        assert_eq!(end["termination"]["signal"], 9);
        assert_eq!(end["termination"]["signalName"], "SIGKILL");
    }

    #[cfg_attr(not(target_os = "linux"), ignore)]
    #[tokio::test]
    async fn connect_reuses_existing_pid() {
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/bin/bash","args":["-c","sleep 2"]},"pty":{"size":{"rows":24,"cols":80}}}"#,
        ));
        let response = start(HeaderMap::new(), body).await;
        let mut stream = response.into_body().into_data_stream();
        let first = stream.next().await.unwrap().unwrap();
        let (_, payload) = decode_frame(&first).unwrap();
        let pid = serde_json::from_slice::<serde_json::Value>(&payload).unwrap()["event"]["start"]
            ["pid"]
            .as_u64()
            .unwrap();

        let request = serde_json::json!({"process":{"pid":pid}});
        let response = connect(bytes::Bytes::from(encode_stream_message(
            &request.to_string().into_bytes(),
        )))
        .await;
        let mut stream = response.into_body().into_data_stream();
        let first = stream.next().await.unwrap().unwrap();
        let (_, payload) = decode_frame(&first).unwrap();
        let connected_pid = serde_json::from_slice::<serde_json::Value>(&payload).unwrap()["event"]
            ["start"]["pid"]
            .as_u64()
            .unwrap();
        assert_eq!(connected_pid, pid);

        let signal = serde_json::json!({"process":{"pid":pid},"signal":"SIGNAL_SIGKILL"});
        assert_eq!(
            send_signal(bytes::Bytes::from(signal.to_string()))
                .await
                .status(),
            StatusCode::OK
        );
    }

    #[tokio::test]
    async fn empty_pty_command_returns_bad_request() {
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":""},"pty":{"size":{"rows":24,"cols":80}}}"#,
        ));
        let response = start(HeaderMap::new(), body).await;
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn connect_unknown_pid_returns_not_found() {
        let request = serde_json::json!({"process":{"pid":987654321}});
        let response = connect(bytes::Bytes::from(encode_stream_message(
            &request.to_string().into_bytes(),
        )))
        .await;
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn send_signal_unknown_pid_returns_not_found() {
        let request = serde_json::json!({"process":{"pid":987654321},"signal":"SIGNAL_SIGKILL"});
        assert_eq!(
            send_signal(bytes::Bytes::from(request.to_string()))
                .await
                .status(),
            StatusCode::NOT_FOUND
        );
    }

    #[tokio::test]
    async fn send_signal_rejects_unsupported_signal() {
        let request = serde_json::json!({"process":{"pid":1},"signal":"SIGNAL_SIGTERM"});
        assert_eq!(
            send_signal(bytes::Bytes::from(request.to_string()))
                .await
                .status(),
            StatusCode::BAD_REQUEST
        );
    }

    #[tokio::test]
    async fn send_input_rejects_invalid_base64() {
        let request = serde_json::json!({"process":{"pid":1},"input":{"pty":"@@@not-base64@@@"}});
        assert_eq!(
            send_input(bytes::Bytes::from(request.to_string()))
                .await
                .status(),
            StatusCode::BAD_REQUEST
        );
    }

    #[tokio::test]
    async fn send_input_requires_exactly_one_arm() {
        for input in [
            serde_json::json!({}),
            serde_json::json!({"stdin":"AA==","pty":"AA=="}),
        ] {
            let request = serde_json::json!({"process":{"pid":1},"input":input});
            assert_eq!(
                send_input(bytes::Bytes::from(request.to_string()))
                    .await
                    .status(),
                StatusCode::NOT_IMPLEMENTED
            );
        }
    }
}

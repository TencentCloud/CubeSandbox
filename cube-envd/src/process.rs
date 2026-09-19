use crate::connect::{decode_frame, encode_end_stream, encode_stream_message};
use crate::defaults::{self, Defaults};
use crate::processes::{self, ProcessRecord};
use crate::termination::{self, CgroupMemoryMonitor, TerminationInfo};
use axum::{
    body::Body,
    extract::Json,
    http::{header, HeaderMap, StatusCode},
    response::Response,
};
use base64::{engine::general_purpose::STANDARD, Engine as _};
use serde::{Deserialize, Serialize};
use std::{convert::Infallible, io, path::Path, process::Stdio, time::Duration};
use tokio::{
    io::{AsyncRead, AsyncReadExt},
    process::{Child, Command},
    sync::mpsc,
    time::{self, Instant},
};
use tokio_stream::{wrappers::ReceiverStream, StreamExt};

const KEEPALIVE_INTERVAL: Duration = Duration::from_secs(30);

#[derive(Debug, Deserialize)]
struct ProcessStartRequest {
    process: ProcessConfig,
    // Upstream defaults stdin to true for backwards compatibility; a piped
    // stdin is exposed through StreamInput/CloseStdin.
    #[serde(default = "default_true", rename = "stdin")]
    stdin: bool,
    #[serde(default)]
    tag: Option<String>,
}

fn default_true() -> bool {
    true
}

#[derive(Debug, Deserialize)]
struct ProcessConfig {
    cmd: String,
    #[serde(default)]
    args: Vec<String>,
    #[serde(default)]
    envs: std::collections::HashMap<String, String>,
    #[serde(default)]
    cwd: Option<String>,
}

#[derive(Debug, Serialize)]
struct ProcessResponse {
    event: ProcessEvent,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct ProcessEvent {
    #[serde(skip_serializing_if = "Option::is_none")]
    start: Option<StartEvent>,
    #[serde(skip_serializing_if = "Option::is_none")]
    data: Option<DataEvent>,
    #[serde(skip_serializing_if = "Option::is_none")]
    end: Option<EndEvent>,
    #[serde(skip_serializing_if = "Option::is_none")]
    keepalive: Option<EmptyEvent>,
}

#[derive(Debug, Serialize)]
struct StartEvent {
    pid: u32,
}

#[derive(Debug, Default, Serialize)]
struct DataEvent {
    #[serde(skip_serializing_if = "Option::is_none")]
    stdout: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    stderr: Option<String>,
}

#[derive(Debug, Serialize)]
struct EndEvent {
    #[serde(rename = "exitCode", skip_serializing_if = "Option::is_none")]
    exit_code: Option<i32>,
    exited: bool,
    status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    termination: Option<TerminationInfo>,
}

#[derive(Debug, Serialize)]
struct EmptyEvent {}

enum ChildOutput {
    Stdout(Vec<u8>),
    Stderr(Vec<u8>),
    ReaderDone,
}

pub async fn start(headers: HeaderMap, body: bytes::Bytes) -> Response {
    let request = match decode_request(&body) {
        Ok(request) => request,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
    };

    let defaults = defaults::snapshot();
    let user = match basic_auth_user(&headers, &defaults) {
        Ok(user) => user,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error),
    };
    let timeout = match connect_timeout(&headers) {
        Ok(timeout) => timeout,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error),
    };

    let record = ProcessRecord {
        cmd: request.process.cmd.clone(),
        args: request.process.args.clone(),
        envs: defaults.merged_env_vars(&request.process.envs),
        cwd: request.process.cwd.clone().or_else(|| defaults.workdir()),
        tag: request.tag.clone(),
    };

    if let Some(cwd) = record.cwd.as_deref() {
        if !Path::new(cwd).is_dir() {
            return error_response(
                StatusCode::BAD_REQUEST,
                format!("cwd '{cwd}' is not a directory"),
            );
        }
    }

    let oom_monitor = CgroupMemoryMonitor::start();
    let mut child = match spawn_process(request, &user, &defaults) {
        Ok(child) => child,
        Err(error) => return error_response(StatusCode::INTERNAL_SERVER_ERROR, error.to_string()),
    };

    let pid = child.id().unwrap_or_default();
    if let Some(writer) = child.stdin.take() {
        processes::register_stdin(pid, writer);
    }
    processes::register(pid, record);
    let (sender, receiver) = mpsc::channel::<Result<bytes::Bytes, Infallible>>(32);
    send_frame(
        &sender,
        ProcessResponse {
            event: ProcessEvent {
                start: Some(StartEvent { pid }),
                data: None,
                end: None,
                keepalive: None,
            },
        },
    )
    .await;

    tokio::spawn(run_process(child, sender, timeout, oom_monitor, pid));

    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/connect+json")
        .body(Body::from_stream(
            ReceiverStream::new(receiver).map(|item| item),
        ))
        .expect("valid process stream response")
}

#[derive(Debug, Default, Deserialize)]
pub(crate) struct ListRequest {}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ProcessInfo {
    config: ProcessInfoConfig,
    pid: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    tag: Option<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ProcessInfoConfig {
    cmd: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    args: Vec<String>,
    #[serde(skip_serializing_if = "std::collections::HashMap::is_empty")]
    envs: std::collections::HashMap<String, String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    cwd: Option<String>,
}

#[derive(Debug, Serialize)]
pub(crate) struct ListResponse {
    processes: Vec<ProcessInfo>,
}

pub(crate) async fn list(Json(_request): Json<ListRequest>) -> Json<ListResponse> {
    let processes = processes::list()
        .into_iter()
        .map(|(pid, record)| ProcessInfo {
            config: ProcessInfoConfig {
                cmd: record.cmd,
                args: record.args,
                envs: record.envs,
                cwd: record.cwd,
            },
            pid,
            tag: record.tag,
        })
        .collect();
    Json(ListResponse { processes })
}

fn decode_request(
    body: &[u8],
) -> Result<ProcessStartRequest, Box<dyn std::error::Error + Send + Sync>> {
    let (flags, payload) = decode_frame(body)?;
    if flags != 0 {
        return Err("Process.Start request must be a regular Connect frame".into());
    }
    Ok(serde_json::from_slice(&payload)?)
}

fn basic_auth_user(headers: &HeaderMap, defaults: &Defaults) -> Result<String, String> {
    let Some(value) = headers.get(header::AUTHORIZATION) else {
        return Ok(defaults.user_or_root());
    };
    let value = value
        .to_str()
        .map_err(|_| "Authorization header is not valid UTF-8".to_owned())?;
    let encoded = value
        .strip_prefix("Basic ")
        .ok_or_else(|| "Authorization must use Basic authentication".to_owned())?;
    let decoded = STANDARD
        .decode(encoded)
        .map_err(|_| "Authorization credentials are not valid base64".to_owned())?;
    let credentials = String::from_utf8(decoded)
        .map_err(|_| "Authorization credentials are not valid UTF-8".to_owned())?;
    let username = credentials
        .split_once(':')
        .map(|(username, _)| username)
        .unwrap_or(credentials.as_str());
    if username.is_empty() {
        Ok(defaults.user_or_root())
    } else {
        Ok(username.to_owned())
    }
}

pub(crate) fn connect_timeout(headers: &HeaderMap) -> Result<Option<Duration>, String> {
    let Some(value) = headers.get("Connect-Timeout-Ms") else {
        return Ok(None);
    };
    let milliseconds = value
        .to_str()
        .map_err(|_| "Connect-Timeout-Ms is not valid UTF-8".to_owned())?
        .parse::<u64>()
        .map_err(|_| "Connect-Timeout-Ms must be an unsigned integer".to_owned())?;
    Ok(Some(Duration::from_millis(milliseconds)))
}

fn spawn_process(
    request: ProcessStartRequest,
    user: &str,
    defaults: &Defaults,
) -> io::Result<Child> {
    let mut command = Command::new(&request.process.cmd);
    command
        .args(&request.process.args)
        .envs(defaults.merged_env_vars(&request.process.envs))
        .stdin(if request.stdin {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    // Start each command in its own process group so a timeout can reap the
    // whole tree, not just the direct child.
    #[cfg(unix)]
    command.process_group(0);
    if let Some(cwd) = request.process.cwd.clone().or_else(|| defaults.workdir()) {
        command.current_dir(cwd);
    }

    #[cfg(unix)]
    if user != "root" {
        use nix::unistd::{Gid, Uid, User};
        use std::ffi::CString;

        let account = User::from_name(user)
            .map_err(|error| io::Error::other(format!("lookup user {user}: {error}")))?
            .ok_or_else(|| {
                io::Error::new(io::ErrorKind::NotFound, format!("user {user} not found"))
            })?;
        let username = CString::new(user)
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "username contains NUL"))?;
        let uid = Uid::from_raw(account.uid.as_raw());
        let gid = Gid::from_raw(account.gid.as_raw());
        unsafe {
            command.pre_exec(move || {
                nix::unistd::initgroups(&username, gid)
                    .map_err(|error| io::Error::other(format!("initgroups failed: {error}")))?;
                nix::unistd::setgid(gid)
                    .map_err(|error| io::Error::other(format!("setgid failed: {error}")))?;
                nix::unistd::setuid(uid)
                    .map_err(|error| io::Error::other(format!("setuid failed: {error}")))?;
                Ok(())
            });
        }
    }

    #[cfg(not(unix))]
    if user != "root" {
        return Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "user switching is only supported on Unix",
        ));
    }

    command.spawn()
}

#[cfg(unix)]
fn kill_process_group(_child: &Child, pid: u32) {
    // The child is its own process-group leader, so -pid signals its whole tree.
    let _ = nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(-(pid as i32)),
        nix::sys::signal::Signal::SIGKILL,
    );
}

#[cfg(not(unix))]
fn kill_process_group(child: &Child, _pid: u32) {
    let _ = child.start_kill();
}

#[cfg(unix)]
fn end_event_fields(
    status: std::process::ExitStatus,
    oom_killed: bool,
) -> (i32, bool, String, Option<String>, TerminationInfo) {
    use std::os::unix::process::ExitStatusExt;

    if let Some(signal) = status.signal() {
        let text = format!("signal: {}", termination::legacy_signal_name(signal));
        (
            -1,
            false,
            text.clone(),
            Some(text),
            termination::from_exit_status(status, oom_killed),
        )
    } else {
        let fields = exit_status_fields(status.code().unwrap_or(-1));
        (
            fields.0,
            fields.1,
            fields.2,
            fields.3,
            termination::from_exit_status(status, false),
        )
    }
}

#[cfg(not(unix))]
fn end_event_fields(
    status: std::process::ExitStatus,
    oom_killed: bool,
) -> (i32, bool, String, Option<String>, TerminationInfo) {
    let fields = exit_status_fields(status.code().unwrap_or(-1));
    (
        fields.0,
        fields.1,
        fields.2,
        fields.3,
        termination::from_exit_status(status, oom_killed),
    )
}

fn exit_status_fields(exit_code: i32) -> (i32, bool, String, Option<String>) {
    let text = format!("exit status {exit_code}");
    (
        exit_code,
        true,
        text.clone(),
        (exit_code != 0).then_some(text),
    )
}

async fn run_process(
    mut child: Child,
    sender: mpsc::Sender<Result<bytes::Bytes, Infallible>>,
    timeout: Option<Duration>,
    oom_monitor: CgroupMemoryMonitor,
    pid: u32,
) {
    let (output_sender, mut output_receiver) = mpsc::channel(16);
    if let Some(stdout) = child.stdout.take() {
        spawn_reader(stdout, true, output_sender.clone());
    }
    if let Some(stderr) = child.stderr.take() {
        spawn_reader(stderr, false, output_sender.clone());
    }
    drop(output_sender);
    let mut readers_done = 0;
    let deadline = timeout.map(|duration| Instant::now() + duration);
    let mut process_done = false;
    let mut keepalive = time::interval(KEEPALIVE_INTERVAL);
    keepalive.set_missed_tick_behavior(time::MissedTickBehavior::Delay);
    keepalive.tick().await;
    let mut timeout_sleep = Box::pin(time::sleep(timeout.unwrap_or(Duration::from_secs(86400))));
    let mut timed_out = false;
    let mut status = None;

    loop {
        tokio::select! {
            result = child.wait(), if !process_done => {
                status = Some(result);
                process_done = true;
            }
            Some(item) = output_receiver.recv(), if readers_done < 2 => {
                match item {
                    ChildOutput::ReaderDone => readers_done += 1,
                    output => send_output(&sender, output).await,
                }
            }
            _ = &mut timeout_sleep, if deadline.is_some() && !process_done && !timed_out => {
                kill_process_group(&child, pid);
                timed_out = true;
            }
            _ = keepalive.tick(), if !process_done => {
                send_frame(&sender, ProcessResponse {
                    event: ProcessEvent { start: None, data: None, end: None, keepalive: Some(EmptyEvent {}) },
                }).await;
            }
        }

        if process_done && readers_done == 2 {
            break;
        }
    }

    let (exit_code, exited, status_text, error, mut termination) = match status {
        Some(Ok(exit_status)) => end_event_fields(exit_status, oom_monitor.was_oom_killed()),
        Some(Err(error)) => (
            -1,
            false,
            error.to_string(),
            Some(error.to_string()),
            termination::TerminationInfo::unknown(),
        ),
        None => {
            let message = "process did not return an exit status".to_owned();
            (
                -1,
                false,
                message.clone(),
                Some(message),
                termination::TerminationInfo::unknown(),
            )
        }
    };
    // The timeout message is cube-envd's own contract (unit-tested); upstream
    // envd surfaces the underlying kill as "signal: killed" instead.
    let error = if timed_out {
        termination = termination::TerminationInfo::timeout();
        Some("process timed out".to_owned())
    } else {
        error
    };
    send_frame(
        &sender,
        ProcessResponse {
            event: ProcessEvent {
                start: None,
                data: None,
                end: Some(EndEvent {
                    exit_code: (!timed_out).then_some(exit_code),
                    exited: !timed_out && exited,
                    status: status_text,
                    error,
                    termination: Some(termination),
                }),
                keepalive: None,
            },
        },
    )
    .await;
    let _ = sender
        .send(Ok(bytes::Bytes::from(encode_end_stream(None))))
        .await;
    processes::close_stdin(pid);
    processes::unregister(pid);
}

fn spawn_reader<R>(mut reader: R, stdout: bool, sender: mpsc::Sender<ChildOutput>)
where
    R: AsyncRead + Unpin + Send + 'static,
{
    tokio::spawn(async move {
        let mut buffer = vec![0u8; 8192];
        loop {
            match reader.read(&mut buffer).await {
                Ok(0) => break,
                Ok(size) => {
                    let data = buffer[..size].to_vec();
                    let item = if stdout {
                        ChildOutput::Stdout(data)
                    } else {
                        ChildOutput::Stderr(data)
                    };
                    if sender.send(item).await.is_err() {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
        let _ = sender.send(ChildOutput::ReaderDone).await;
    });
}

async fn send_output(sender: &mpsc::Sender<Result<bytes::Bytes, Infallible>>, output: ChildOutput) {
    let mut data = DataEvent::default();
    match output {
        ChildOutput::Stdout(bytes) => data.stdout = Some(STANDARD.encode(bytes)),
        ChildOutput::Stderr(bytes) => data.stderr = Some(STANDARD.encode(bytes)),
        ChildOutput::ReaderDone => return,
    }
    send_frame(
        sender,
        ProcessResponse {
            event: ProcessEvent {
                start: None,
                data: Some(data),
                end: None,
                keepalive: None,
            },
        },
    )
    .await;
}

async fn send_frame<T: Serialize>(
    sender: &mpsc::Sender<Result<bytes::Bytes, Infallible>>,
    value: T,
) {
    if let Ok(payload) = serde_json::to_vec(&value) {
        let _ = sender
            .send(Ok(bytes::Bytes::from(encode_stream_message(&payload))))
            .await;
    }
}

fn error_response(status: StatusCode, message: String) -> Response {
    let payload = serde_json::json!({
        "code": "process_error",
        "message": message,
    });
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(payload.to_string()))
        .expect("valid process error response")
}

fn unimplemented_response(message: String) -> Response {
    let payload = serde_json::json!({
        "code": "unimplemented",
        "message": message,
    });
    Response::builder()
        .status(StatusCode::NOT_IMPLEMENTED)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(payload.to_string()))
        .expect("valid process error response")
}

fn empty_response() -> Response {
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from("{}"))
        .expect("valid empty response")
}

#[derive(Debug, Deserialize)]
struct ProcessSelectorRequest {
    process: ProcessSelector,
}

#[derive(Debug, Deserialize)]
struct ProcessSelector {
    #[serde(default)]
    pid: u32,
    #[serde(default)]
    tag: Option<String>,
}

impl ProcessSelector {
    /// Resolve to a pid: an explicit `pid` wins, otherwise look the `tag` up
    /// in the process registry.
    fn resolve(&self) -> Option<u32> {
        if self.pid != 0 {
            return Some(self.pid);
        }
        let tag = self.tag.as_deref()?;
        processes::pid_for_tag(tag)
    }
}

/// `process.Process/StreamInput`: a client stream of `StreamInputRequest`
/// events (`start{process}` → `data{input}`), answered with one empty
/// `StreamInputResponse`. Each `ProcessInput` must set exactly one arm.
pub async fn stream_input(body: bytes::Bytes) -> Response {
    let mut cursor = 0usize;
    let mut target: Option<u32> = None;

    while cursor < body.len() {
        if body.len() - cursor < 5 {
            return error_response(
                StatusCode::BAD_REQUEST,
                "truncated Connect frame".to_owned(),
            );
        }
        let flags = body[cursor];
        let size = u32::from_be_bytes(
            body[cursor + 1..cursor + 5]
                .try_into()
                .expect("frame header"),
        ) as usize;
        let frame_end = cursor + 5 + size;
        if frame_end > body.len() {
            return error_response(
                StatusCode::BAD_REQUEST,
                "truncated Connect frame".to_owned(),
            );
        }
        let payload = &body[cursor + 5..frame_end];
        cursor = frame_end;

        if flags & crate::connect::END_STREAM_FLAG != 0 {
            break;
        }

        let value: serde_json::Value = match serde_json::from_slice(payload) {
            Ok(value) => value,
            Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
        };
        let event = &value["event"];

        if let Some(start) = event.get("start") {
            if let Ok(selector) =
                serde_json::from_value::<ProcessSelector>(start["process"].clone())
            {
                target = selector.resolve();
            }
            continue;
        }
        if event.get("keepalive").is_some() {
            continue;
        }
        let Some(data) = event.get("data") else {
            return unimplemented_response(
                "StreamInput event must carry start, data, or keepalive".to_owned(),
            );
        };

        let input = &data["input"];
        let stdin_arm = input.get("stdin").and_then(|value| value.as_str());
        let pty_arm = input.get("pty").and_then(|value| value.as_str());
        if stdin_arm.is_some() as u8 + pty_arm.is_some() as u8 != 1 {
            return unimplemented_response(
                "StreamInput ProcessInput must set exactly one arm".to_owned(),
            );
        }

        let Some(pid) = target else {
            return unimplemented_response(
                "StreamInput data arrived before the start event".to_owned(),
            );
        };
        let Some(encoded) = stdin_arm.or(pty_arm) else {
            unreachable!("exactly one arm is set");
        };
        let bytes = match STANDARD.decode(encoded) {
            Ok(bytes) => bytes,
            Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
        };

        let written = if stdin_arm.is_some() {
            processes::write_stdin(pid, &bytes).await
        } else {
            crate::pty::write_pty_input(pid, &bytes).map_err(io::Error::other)
        };
        match written {
            Ok(true) => {}
            Ok(false) => {
                return unimplemented_response(format!("no stdin/pty for process {pid}"));
            }
            Err(error) => {
                return error_response(StatusCode::INTERNAL_SERVER_ERROR, error.to_string());
            }
        }
    }

    empty_response()
}

/// `process.Process/CloseStdin`: drop the piped stdin so the process reads EOF.
pub async fn close_stdin(body: bytes::Bytes) -> Response {
    let request = match serde_json::from_slice::<ProcessSelectorRequest>(&body) {
        Ok(request) => request,
        Err(error) => return error_response(StatusCode::BAD_REQUEST, error.to_string()),
    };
    let Some(pid) = request.process.resolve() else {
        return error_response(
            StatusCode::BAD_REQUEST,
            "process pid or tag is required".to_owned(),
        );
    };
    if processes::close_stdin(pid) {
        empty_response()
    } else {
        unimplemented_response(format!("no stdin for process {pid}"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    async fn collect_response(response: Response) -> Vec<serde_json::Value> {
        let mut body = response.into_body().into_data_stream();
        let mut frames = Vec::new();
        while let Some(Ok(chunk)) = body.next().await {
            let (flags, payload) = decode_frame(&chunk).unwrap();
            if flags == 0 {
                frames.push(serde_json::from_slice(&payload).unwrap());
            }
        }
        frames
    }

    #[tokio::test]
    async fn command_stream_contains_start_data_and_end() {
        let mut headers = HeaderMap::new();
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Basic cm9vdDo="),
        );
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/bin/bash","args":["-c","echo -n hello; echo -n world >&2; exit 42"]},"stdin":false}"#,
        ));
        let frames = collect_response(start(headers, body).await).await;
        assert!(
            frames.first().unwrap()["event"]["start"]["pid"]
                .as_u64()
                .unwrap()
                > 0
        );
        let combined = frames[1..frames.len() - 1].iter().fold(
            (String::new(), String::new()),
            |mut output, frame| {
                if let Some(value) = frame["event"]["data"]["stdout"].as_str() {
                    output
                        .0
                        .push_str(&String::from_utf8(STANDARD.decode(value).unwrap()).unwrap());
                }
                if let Some(value) = frame["event"]["data"]["stderr"].as_str() {
                    output
                        .1
                        .push_str(&String::from_utf8(STANDARD.decode(value).unwrap()).unwrap());
                }
                output
            },
        );
        assert_eq!(combined, ("hello".to_owned(), "world".to_owned()));
        assert_eq!(frames.last().unwrap()["event"]["end"]["exitCode"], 42);
        assert_eq!(
            frames.last().unwrap()["event"]["end"]["status"],
            "exit status 42"
        );
        assert_eq!(
            frames.last().unwrap()["event"]["end"]["error"],
            "exit status 42"
        );
        assert_eq!(
            frames.last().unwrap()["event"]["end"]["termination"]["reason"],
            "exited"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn signaled_process_reports_go_style_signal_status() {
        let headers = HeaderMap::new();
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/bin/sh","args":["-c","kill -9 $$"]}}"#,
        ));
        let frames = collect_response(start(headers, body).await).await;
        let end = &frames.last().unwrap()["event"]["end"];
        assert_eq!(end["exitCode"], -1);
        assert_eq!(end["exited"], false);
        assert_eq!(end["status"], "signal: killed");
        assert_eq!(end["error"], "signal: killed");
        assert_eq!(end["termination"]["reason"], "signal");
        assert_eq!(end["termination"]["signal"], 9);
        assert_eq!(end["termination"]["signalName"], "SIGKILL");
    }

    #[tokio::test]
    async fn timeout_kills_process_and_reports_error() {
        let mut headers = HeaderMap::new();
        headers.insert("Connect-Timeout-Ms", HeaderValue::from_static("10"));
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/bin/bash","args":["-c","sleep 2"]}}"#,
        ));
        let frames = collect_response(start(headers, body).await).await;
        assert_eq!(
            frames.last().unwrap()["event"]["end"]["error"],
            "process timed out"
        );
        assert!(frames.last().unwrap()["event"]["end"]["exitCode"].is_null());
        assert_eq!(
            frames.last().unwrap()["event"]["end"]["status"],
            "signal: killed"
        );
        assert_eq!(
            frames.last().unwrap()["event"]["end"]["termination"]["reason"],
            "timeout"
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn user_auth_runs_process_as_user_account_when_available() {
        if nix::unistd::User::from_name("user").unwrap().is_none() {
            return;
        }

        let mut headers = HeaderMap::new();
        headers.insert(
            header::AUTHORIZATION,
            HeaderValue::from_static("Basic dXNlcjo="),
        );
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/bin/bash","args":["-c","id -u"]}}"#,
        ));
        let frames = collect_response(start(headers, body).await).await;
        let stdout = frames
            .iter()
            .filter_map(|frame| frame["event"]["data"]["stdout"].as_str())
            .map(|value| String::from_utf8(STANDARD.decode(value).unwrap()).unwrap())
            .collect::<String>();
        assert_eq!(stdout.trim(), "1000");
    }

    #[tokio::test]
    async fn successful_command_omits_error_field() {
        let body = bytes::Bytes::from(encode_stream_message(br#"{"process":{"cmd":"/bin/true"}}"#));
        let frames = collect_response(start(HeaderMap::new(), body).await).await;
        let end = &frames.last().unwrap()["event"]["end"];
        assert_eq!(end["exitCode"], 0);
        assert_eq!(end["exited"], true);
        assert!(end.get("error").is_none());
    }

    #[tokio::test]
    async fn stderr_only_output_stays_on_stderr() {
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/bin/sh","args":["-c","echo err 1>&2"]}}"#,
        ));
        let frames = collect_response(start(HeaderMap::new(), body).await).await;
        let mut stdout = String::new();
        let mut stderr = String::new();
        for frame in &frames[1..frames.len() - 1] {
            if let Some(value) = frame["event"]["data"]["stdout"].as_str() {
                stdout.push_str(&String::from_utf8(STANDARD.decode(value).unwrap()).unwrap());
            }
            if let Some(value) = frame["event"]["data"]["stderr"].as_str() {
                stderr.push_str(&String::from_utf8(STANDARD.decode(value).unwrap()).unwrap());
            }
        }
        assert!(stdout.is_empty());
        assert_eq!(stderr, "err\n");
    }

    #[tokio::test]
    async fn malformed_request_returns_bad_request() {
        let response = start(HeaderMap::new(), bytes::Bytes::from_static(b"not-a-frame")).await;
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn end_stream_flag_in_request_is_rejected() {
        let body = bytes::Bytes::from(crate::connect::encode_end_stream(None));
        let response = start(HeaderMap::new(), body).await;
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn invalid_timeout_header_returns_bad_request() {
        let mut headers = HeaderMap::new();
        headers.insert("Connect-Timeout-Ms", HeaderValue::from_static("abc"));
        let body = bytes::Bytes::from(encode_stream_message(br#"{"process":{"cmd":"/bin/true"}}"#));
        assert_eq!(start(headers, body).await.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn missing_binary_returns_internal_error() {
        let body = bytes::Bytes::from(encode_stream_message(
            br#"{"process":{"cmd":"/nonexistent/cube-envd-binary"}}"#,
        ));
        assert_eq!(
            start(HeaderMap::new(), body).await.status(),
            StatusCode::INTERNAL_SERVER_ERROR
        );
    }

    #[tokio::test]
    async fn invalid_cwd_returns_bad_request() {
        let payload =
            serde_json::json!({"process":{"cmd":"/bin/true","cwd":"/nonexistent/cube-envd-dir"}});
        let body = bytes::Bytes::from(encode_stream_message(
            &serde_json::to_vec(&payload).unwrap(),
        ));
        assert_eq!(
            start(HeaderMap::new(), body).await.status(),
            StatusCode::BAD_REQUEST
        );
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn timeout_kills_the_process_group() {
        use nix::errno::Errno;
        use nix::sys::signal::kill;
        use nix::unistd::Pid;

        let directory = tempfile::tempdir().unwrap();
        let pidfile = directory.path().join("grandchild.pid");
        let script = format!("sleep 30 & echo $! > {}; wait", pidfile.display());
        let payload = serde_json::json!({
            "process": {"cmd": "/bin/bash", "args": ["-c", script]}
        });
        let mut headers = HeaderMap::new();
        headers.insert("Connect-Timeout-Ms", HeaderValue::from_static("200"));
        let body = bytes::Bytes::from(encode_stream_message(
            &serde_json::to_vec(&payload).unwrap(),
        ));
        let frames = collect_response(start(headers, body).await).await;
        assert_eq!(
            frames.last().unwrap()["event"]["end"]["termination"]["reason"],
            "timeout"
        );

        let grandchild = std::fs::read_to_string(&pidfile)
            .expect("grandchild pid file exists")
            .trim()
            .parse::<i32>()
            .expect("grandchild pid parses");

        let mut gone = false;
        for _ in 0..40 {
            match kill(Pid::from_raw(grandchild), None) {
                Err(Errno::ESRCH) => {
                    gone = true;
                    break;
                }
                _ => tokio::time::sleep(Duration::from_millis(50)).await,
            }
        }
        assert!(gone, "grandchild {grandchild} survived the group kill");
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn stream_input_feeds_piped_stdin_and_close_delivers_eof() {
        let body = bytes::Bytes::from(encode_stream_message(br#"{"process":{"cmd":"/bin/cat"}}"#));
        let response = start(HeaderMap::new(), body).await;
        assert_eq!(response.status(), StatusCode::OK);
        let mut stream = response.into_body().into_data_stream();

        let first = stream.next().await.unwrap().unwrap();
        let (_, payload) = decode_frame(&first).unwrap();
        let pid = serde_json::from_slice::<serde_json::Value>(&payload).unwrap()["event"]["start"]
            ["pid"]
            .as_u64()
            .unwrap() as u32;

        let start_event = serde_json::json!({"event":{"start":{"process":{"pid":pid}}}});
        let data_event =
            serde_json::json!({"event":{"data":{"input":{"stdin": STANDARD.encode("hello\n")}}}});
        let mut input = encode_stream_message(&serde_json::to_vec(&start_event).unwrap());
        input.extend(encode_stream_message(
            &serde_json::to_vec(&data_event).unwrap(),
        ));
        let input_response = stream_input(bytes::Bytes::from(input)).await;
        assert_eq!(input_response.status(), StatusCode::OK);

        let close = serde_json::to_vec(&serde_json::json!({"process":{"pid":pid}})).unwrap();
        assert_eq!(
            close_stdin(bytes::Bytes::from(close)).await.status(),
            StatusCode::OK
        );

        let mut stdout = Vec::new();
        while let Some(Ok(chunk)) = stream.next().await {
            let (flags, payload) = decode_frame(&chunk).unwrap();
            if flags == 0 {
                let frame: serde_json::Value = serde_json::from_slice(&payload).unwrap();
                if let Some(encoded) = frame["event"]["data"]["stdout"].as_str() {
                    stdout.extend(STANDARD.decode(encoded).unwrap());
                }
            }
        }
        assert_eq!(String::from_utf8(stdout).unwrap(), "hello\n");
    }

    #[tokio::test]
    async fn stream_input_rejects_multiple_input_arms() {
        let start_event = serde_json::json!({"event":{"start":{"process":{"pid":1}}}});
        let data_event = serde_json::json!({
            "event":{"data":{"input":{"stdin":"AA==","pty":"AA=="}}}
        });
        let mut input = encode_stream_message(&serde_json::to_vec(&start_event).unwrap());
        input.extend(encode_stream_message(
            &serde_json::to_vec(&data_event).unwrap(),
        ));
        let response = stream_input(bytes::Bytes::from(input)).await;
        assert_eq!(response.status(), StatusCode::NOT_IMPLEMENTED);
    }

    #[tokio::test]
    async fn close_stdin_without_writer_is_unimplemented() {
        let body = serde_json::to_vec(&serde_json::json!({"process":{"pid":987654321}})).unwrap();
        assert_eq!(
            close_stdin(bytes::Bytes::from(body)).await.status(),
            StatusCode::NOT_IMPLEMENTED
        );
    }
}

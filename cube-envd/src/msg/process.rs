// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Hand-written serde mappings for `spec/process/process.proto` following
//! the proto3 JSON convention as implemented by upstream envd
//! (baseline-verified quirks):
//! - field names are camelCase on output; input accepts snake_case aliases;
//! - `bytes` fields are base64 strings;
//! - default values are omitted on output (`exitCode: 0` disappears — SDKs
//!   recover it from `status: "exit status 0"`);
//! - the `ProcessSelector` oneof is FLAT in JSON: `{"process":{"pid":1}}`
//!   or `{"process":{"tag":"t"}}`, never `{"process":{"selector":{...}}}`.

use std::collections::HashMap;

use serde::{Deserialize, Serialize};

fn is_false(b: &bool) -> bool {
    !*b
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct ProcessConfig {
    #[serde(default)]
    pub cmd: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    pub envs: HashMap<String, String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cwd: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct PtySize {
    #[serde(default)]
    pub cols: u32,
    #[serde(default)]
    pub rows: u32,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Pty {
    #[serde(default)]
    pub size: Option<PtySize>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct StartRequest {
    pub process: ProcessConfig,
    #[serde(default)]
    pub pty: Option<Pty>,
    #[serde(default)]
    pub tag: Option<String>,
    #[serde(default)]
    pub stdin: Option<bool>,
}

/// Flat oneof selector: `{"pid":1}` or `{"tag":"t"}`.
///
/// The nested `{"selector":{...}}` shape is deliberately NOT understood.
/// Upstream Go envd rejects that shape outright; a nested-only selector
/// is rejected before control dispatch (#1227: no destructive side effects).
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProcessSelector {
    #[serde(default)]
    pub pid: Option<u32>,
    #[serde(default)]
    pub tag: Option<String>,
}

impl ProcessSelector {
    pub fn flatten(&self) -> (Option<u32>, Option<String>) {
        (self.pid, self.tag.clone())
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct SendSignalRequest {
    pub process: ProcessSelector,
    /// proto3 JSON allows an enum either by name ("SIGNAL_SIGKILL") or by
    /// number (9); accept both shapes like the Go decoder does.
    #[serde(default)]
    pub signal: Option<serde_json::Value>,
}

/// Input bytes use protobuf JSON's base64 string representation. The service
/// validates that exactly one oneof arm is present before decoding it.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct ProcessInput {
    #[serde(default)]
    pub stdin: Option<String>,
    #[serde(default)]
    pub pty: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct SendInputRequest {
    pub process: ProcessSelector,
    #[serde(default)]
    pub input: ProcessInput,
}

#[derive(Debug, Clone, Deserialize)]
pub struct CloseStdinRequest {
    pub process: ProcessSelector,
}

/// Client-streaming input event. The flattened optional fields mirror the
/// protobuf oneof; the handler rejects zero or multiple populated arms.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct StreamInputRequest {
    #[serde(default)]
    pub start: Option<StreamInputStartEvent>,
    #[serde(default)]
    pub data: Option<StreamInputDataEvent>,
    #[serde(default)]
    pub keepalive: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct StreamInputStartEvent {
    pub process: ProcessSelector,
}

#[derive(Debug, Clone, Default, Deserialize)]
pub struct StreamInputDataEvent {
    #[serde(default)]
    pub input: ProcessInput,
}

/// `process.Process/Connect`: attach to an already-running process selected by
/// pid or tag. Server-streaming; the response is the same `Event` stream as
/// Start (start → data/keepalive → end), except it begins at the current head
/// of the output bus — nothing before the attach is replayed.
#[derive(Debug, Clone, Deserialize)]
pub struct ConnectRequest {
    pub process: ProcessSelector,
}

/// `process.Process/Update`: resize the pty window of a running process. The
/// `pty` is optional in the proto; the handler resolves the process first, then
/// a missing `pty` (or a `pty` without a `size`) is a silent no-op success —
/// matching Go envd — not a caller error.
#[derive(Debug, Clone, Deserialize)]
pub struct UpdateRequest {
    pub process: ProcessSelector,
    #[serde(default)]
    pub pty: Option<Pty>,
}

// ---------- responses / events ----------

#[derive(Debug, Clone, Serialize)]
pub struct ProcessInfo {
    pub config: ProcessConfig,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pid: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tag: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ListResponse {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub processes: Vec<ProcessInfo>,
}

/// ProcessEvent oneof, flattened per proto3 JSON.
#[derive(Debug, Clone, Serialize)]
pub struct EventEnvelope {
    pub event: Event,
}

#[derive(Debug, Clone, Serialize)]
pub enum Event {
    #[serde(rename = "start")]
    Start(StartEvent),
    #[serde(rename = "data")]
    Data(DataEvent),
    #[serde(rename = "end")]
    End(EndEvent),
    #[serde(rename = "keepalive")]
    KeepAlive(serde_json::Map<String, serde_json::Value>),
}

#[derive(Debug, Clone, Serialize)]
pub struct StartEvent {
    pub pid: u32,
}

/// DataEvent output oneof — exactly one of the fields set, base64 payload.
#[derive(Debug, Clone, Default, Serialize)]
pub struct DataEvent {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stdout: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stderr: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pty: Option<String>,
}

/// Baseline-verified shapes:
/// - exit 0:      {"exited":true,"status":"exit status 0"}
/// - exit N != 0: {"exitCode":N,"exited":true,"status":"exit status N","error":"exit status N"}
/// - signal:      {"exitCode":-1,"status":"signal: killed","error":"signal: killed"}
#[derive(Debug, Clone, Serialize)]
pub struct EndEvent {
    #[serde(rename = "exitCode", skip_serializing_if = "is_zero_i32")]
    pub exit_code: i32,
    #[serde(skip_serializing_if = "is_false")]
    pub exited: bool,
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub signal: Option<i32>,
    #[serde(rename = "oomKilled", skip_serializing_if = "Option::is_none")]
    pub oom_killed: Option<bool>,
    #[serde(rename = "killedBy", skip_serializing_if = "Option::is_none")]
    pub killed_by: Option<String>,
}

fn is_zero_i32(v: &i32) -> bool {
    *v == 0
}

impl EndEvent {
    pub fn from_exit_status(status: std::process::ExitStatus) -> Self {
        use std::os::unix::process::ExitStatusExt;
        if let Some(code) = status.code() {
            let text = format!("exit status {code}");
            EndEvent {
                exit_code: code,
                exited: true,
                status: text.clone(),
                error: (code != 0).then_some(text),
                signal: None,
                oom_killed: None,
                killed_by: None,
            }
        } else {
            let signo = status.signal().unwrap_or(0);
            let text = format!("signal: {}", signal_name(signo));
            EndEvent {
                exit_code: -1,
                exited: false,
                status: text.clone(),
                error: Some(text),
                signal: Some(signo),
                oom_killed: None,
                killed_by: None,
            }
        }
    }
}

/// Go `os/exec.ProcessState.String()` vocabulary for common signals.
fn signal_name(signo: i32) -> String {
    match signo {
        libc::SIGHUP => "hangup".to_string(),
        libc::SIGINT => "interrupt".to_string(),
        libc::SIGQUIT => "quit".to_string(),
        libc::SIGABRT => "aborted".to_string(),
        libc::SIGKILL => "killed".to_string(),
        libc::SIGSEGV => "segmentation fault".to_string(),
        libc::SIGPIPE => "broken pipe".to_string(),
        libc::SIGTERM => "terminated".to_string(),
        other => format!("signal {other}"),
    }
}

/// Parse the Signal enum from its proto3 JSON name or number (both are
/// valid proto3 JSON encodings of an enum).
pub fn parse_signal(value: Option<&serde_json::Value>) -> Option<i32> {
    match value? {
        serde_json::Value::String(s) => match s.as_str() {
            "SIGNAL_SIGKILL" | "9" => Some(libc::SIGKILL),
            "SIGNAL_SIGTERM" | "15" => Some(libc::SIGTERM),
            _ => None,
        },
        serde_json::Value::Number(n) => match n.as_i64() {
            Some(9) => Some(libc::SIGKILL),
            Some(15) => Some(libc::SIGTERM),
            _ => None,
        },
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn start_request_accepts_sdk_payload() {
        // Exactly what the Python SDK fallback sends.
        let raw = r#"{"process":{"cmd":"/bin/bash","args":["-l","-c","echo hi"],"envs":{"A":"1"}},"stdin":false}"#;
        let req: StartRequest = serde_json::from_str(raw).unwrap();
        assert_eq!(req.process.cmd, "/bin/bash");
        assert_eq!(req.process.args.len(), 3);
        assert_eq!(req.stdin, Some(false));
        assert!(req.pty.is_none());
    }

    #[test]
    fn input_requests_accept_proto_json_shapes() {
        let unary: SendInputRequest =
            serde_json::from_str(r#"{"process":{"pid":42},"input":{"pty":"aGkK"}}"#).unwrap();
        assert_eq!(unary.process.pid, Some(42));
        assert_eq!(unary.input.pty.as_deref(), Some("aGkK"));

        let start: StreamInputRequest =
            serde_json::from_str(r#"{"start":{"process":{"tag":"shell"}}}"#).unwrap();
        assert_eq!(
            start.start.expect("start event").process.tag.as_deref(),
            Some("shell")
        );
        let keepalive: StreamInputRequest = serde_json::from_str(r#"{"keepalive":{}}"#).unwrap();
        assert!(keepalive.keepalive.is_some());
    }

    #[test]
    fn selector_flat_only_nested_rejected() {
        let flat: ProcessSelector = serde_json::from_str(r#"{"tag":"t1"}"#).unwrap();
        assert_eq!(flat.flatten(), (None, Some("t1".to_string())));
        let flat_pid: ProcessSelector = serde_json::from_str(r#"{"pid":42}"#).unwrap();
        assert_eq!(flat_pid.flatten(), (Some(42), None));
        assert!(serde_json::from_str::<ProcessSelector>(r#"{"selector":{"pid":7}}"#).is_err());
    }

    #[test]
    fn end_event_exit_zero_omits_defaults() {
        let e = EndEvent {
            exit_code: 0,
            exited: true,
            status: "exit status 0".into(),
            error: None,
            signal: None,
            oom_killed: None,
            killed_by: None,
        };
        let v = serde_json::to_value(EventEnvelope {
            event: Event::End(e),
        })
        .unwrap();
        assert_eq!(
            v,
            serde_json::json!({"event":{"end":{"exited":true,"status":"exit status 0"}}})
        );
    }

    #[test]
    fn termination_metadata_uses_proto_json_field_names() {
        use std::os::unix::process::ExitStatusExt;

        let mut end = EndEvent::from_exit_status(std::process::ExitStatus::from_raw(libc::SIGKILL));
        end.oom_killed = Some(true);
        end.killed_by = Some("oom".to_string());
        let value = serde_json::to_value(end).unwrap();
        assert_eq!(value["signal"], 9);
        assert_eq!(value["oomKilled"], true);
        assert_eq!(value["killedBy"], "oom");
        assert!(value.get("oom_killed").is_none());
        assert!(value.get("killed_by").is_none());
    }

    #[test]
    fn end_event_nonzero_and_signal() {
        let e = EndEvent {
            exit_code: 3,
            exited: true,
            status: "exit status 3".into(),
            error: Some("exit status 3".into()),
            signal: None,
            oom_killed: None,
            killed_by: None,
        };
        let v = serde_json::to_value(&e).unwrap();
        assert_eq!(v["exitCode"], 3);
        assert_eq!(v["error"], "exit status 3");

        let killed = EndEvent {
            exit_code: -1,
            exited: false,
            status: "signal: killed".into(),
            error: Some("signal: killed".into()),
            signal: Some(9),
            oom_killed: None,
            killed_by: None,
        };
        let v = serde_json::to_value(&killed).unwrap();
        assert_eq!(v["exitCode"], -1);
        assert!(v.get("exited").is_none(), "exited:false must be omitted");
        assert_eq!(v["status"], "signal: killed");
    }

    #[test]
    fn list_response_empty_is_empty_object() {
        let v = serde_json::to_value(ListResponse { processes: vec![] }).unwrap();
        assert_eq!(v, serde_json::json!({}));
    }

    #[test]
    fn data_event_base64_oneof() {
        let v = serde_json::to_value(EventEnvelope {
            event: Event::Data(DataEvent {
                stdout: Some("aGk=".into()),
                ..Default::default()
            }),
        })
        .unwrap();
        assert_eq!(v, serde_json::json!({"event":{"data":{"stdout":"aGk="}}}));
    }

    #[test]
    fn signal_parsing() {
        let sk = serde_json::json!("SIGNAL_SIGKILL");
        let st = serde_json::json!("SIGNAL_SIGTERM");
        let su = serde_json::json!("SIGNAL_UNSPECIFIED");
        let n9 = serde_json::json!(9);
        let n15 = serde_json::json!(15);
        let n0 = serde_json::json!(0);
        assert_eq!(parse_signal(Some(&sk)), Some(libc::SIGKILL));
        assert_eq!(parse_signal(Some(&st)), Some(libc::SIGTERM));
        assert_eq!(parse_signal(Some(&su)), None);
        // proto3 JSON numeric enum encoding.
        assert_eq!(parse_signal(Some(&n9)), Some(libc::SIGKILL));
        assert_eq!(parse_signal(Some(&n15)), Some(libc::SIGTERM));
        assert_eq!(parse_signal(Some(&n0)), None);
        assert_eq!(parse_signal(None), None);
    }
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::{anyhow, Context};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use reqwest::header::{HeaderMap, HeaderValue};
use serde_json::json;
use std::time::Duration;

const CONNECT_PROTOCOL_VERSION: &str = "1";
const CONNECT_CONTENT_TYPE: &str = "application/connect+json";
const CONNECT_COMPRESSED_FLAG: u8 = 0x01;
const CONNECT_END_STREAM_FLAG: u8 = 0x02;
const MAX_CONNECT_ENVELOPE_SIZE: usize = 64 * 1024 * 1024;
const ENVD_PORT: u16 = 49983;
const DEFAULT_USER: &str = "root";

#[derive(Debug, Clone)]
pub struct TerminalTarget {
    pub sandbox_id: String,
    pub domain: String,
    pub envd_access_token: Option<String>,
    pub container_id: Option<String>,
    pub process_base_url: Option<String>,
}

impl TerminalTarget {
    pub fn envd_base_url(&self) -> String {
        if let Some(base_url) = self.process_base_url.as_deref().filter(|v| !v.is_empty()) {
            return base_url.trim_end_matches('/').to_string();
        }
        format!(
            "http://{}-{}.{}/process.Process",
            ENVD_PORT, self.sandbox_id, self.domain
        )
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PtyEvent {
    Start {
        pid: i64,
    },
    Output {
        data: Vec<u8>,
    },
    End {
        code: Option<i32>,
        message: Option<String>,
    },
    Ignored,
}

#[derive(Debug)]
pub struct ConnectJsonDecoder {
    buffer: Vec<u8>,
}

impl ConnectJsonDecoder {
    pub fn new() -> Self {
        Self { buffer: Vec::new() }
    }

    pub fn push(&mut self, chunk: &[u8]) -> anyhow::Result<Vec<serde_json::Value>> {
        self.buffer.extend_from_slice(chunk);
        let mut out = Vec::new();
        while self.buffer.len() >= 5 {
            let flags = self.buffer[0];
            let size = u32::from_be_bytes([
                self.buffer[1],
                self.buffer[2],
                self.buffer[3],
                self.buffer[4],
            ]) as usize;
            if size > MAX_CONNECT_ENVELOPE_SIZE {
                return Err(anyhow!("Connect stream message too large: {} bytes", size));
            }
            if self.buffer.len() < 5 + size {
                break;
            }
            let raw = self.buffer[5..5 + size].to_vec();
            self.buffer.drain(..5 + size);

            if flags & CONNECT_COMPRESSED_FLAG != 0 {
                return Err(anyhow!("unsupported compressed Connect stream message"));
            }
            if flags & CONNECT_END_STREAM_FLAG != 0 {
                if !raw.is_empty() {
                    let trailer: serde_json::Value =
                        serde_json::from_slice(&raw).context("invalid Connect end stream JSON")?;
                    if trailer.get("error").is_some() {
                        return Err(anyhow!("Connect stream error: {}", trailer));
                    }
                }
                continue;
            }
            out.push(serde_json::from_slice(&raw).context("invalid Connect JSON frame")?);
        }
        Ok(out)
    }
}

pub fn encode_connect_envelope(body: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(5 + body.len());
    out.push(0);
    out.extend_from_slice(&(body.len() as u32).to_be_bytes());
    out.extend_from_slice(body);
    out
}

pub fn parse_pty_event(message: serde_json::Value) -> anyhow::Result<PtyEvent> {
    let Some(event) = message.get("event") else {
        return Ok(PtyEvent::Ignored);
    };
    if let Some(start) = event.get("start") {
        if let Some(pid) = start.get("pid").and_then(|v| v.as_i64()) {
            return Ok(PtyEvent::Start { pid });
        }
        return Err(anyhow!("PTY start event missing pid"));
    }
    if let Some(data) = event.get("data") {
        if let Some(pty) = data.get("pty").and_then(|v| v.as_str()) {
            return Ok(PtyEvent::Output {
                data: BASE64.decode(pty).context("invalid PTY base64 data")?,
            });
        }
    }
    if let Some(end) = event.get("end") {
        let code = end
            .get("exitCode")
            .or_else(|| end.get("exit_code"))
            .and_then(|v| v.as_i64())
            .map(|v| v as i32);
        let message = end
            .get("error")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        return Ok(PtyEvent::End { code, message });
    }
    Ok(PtyEvent::Ignored)
}

#[derive(Clone)]
pub struct TerminalService {
    client: reqwest::Client,
}

impl TerminalService {
    pub fn new(client: reqwest::Client) -> Self {
        Self { client }
    }

    pub fn start_payload(rows: u16, cols: u16) -> serde_json::Value {
        json!({
            "process": {
                "cmd": "/bin/bash",
                "args": ["-i", "-l"],
                "envs": {
                    "TERM": "xterm-256color",
                    "LANG": "C.UTF-8",
                    "LC_ALL": "C.UTF-8"
                }
            },
            "pty": {
                "size": {
                    "rows": rows.max(1),
                    "cols": cols.max(1)
                }
            }
        })
    }

    pub async fn start(
        &self,
        target: &TerminalTarget,
        rows: u16,
        cols: u16,
    ) -> anyhow::Result<reqwest::Response> {
        let body = serde_json::to_vec(&Self::start_payload(rows, cols))?;
        let resp = self
            .client
            .post(format!("{}/Start", target.envd_base_url()))
            .headers(streaming_headers(target)?)
            .body(encode_connect_envelope(&body))
            .send()
            .await
            .context("failed to start envd PTY")?;
        if !resp.status().is_success() {
            return Err(anyhow!("envd PTY start failed with HTTP {}", resp.status()));
        }
        Ok(resp)
    }

    pub async fn send_input(
        &self,
        target: &TerminalTarget,
        pid: i64,
        data: &[u8],
    ) -> anyhow::Result<()> {
        self.unary(target, "SendInput", Self::input_payload(pid, data))
            .await
    }

    pub async fn resize(
        &self,
        target: &TerminalTarget,
        pid: i64,
        rows: u16,
        cols: u16,
    ) -> anyhow::Result<()> {
        self.unary(target, "Update", Self::resize_payload(pid, rows, cols))
            .await
    }

    pub async fn kill(&self, target: &TerminalTarget, pid: i64) -> anyhow::Result<()> {
        self.unary(target, "SendSignal", Self::kill_payload(pid))
            .await
    }

    pub fn input_payload(pid: i64, data: &[u8]) -> serde_json::Value {
        json!({
            "process": {"pid": pid},
            "input": {"pty": BASE64.encode(data)}
        })
    }

    pub fn resize_payload(pid: i64, rows: u16, cols: u16) -> serde_json::Value {
        json!({
            "process": {"pid": pid},
            "pty": {"size": {"rows": rows.max(1), "cols": cols.max(1)}}
        })
    }

    pub fn kill_payload(pid: i64) -> serde_json::Value {
        json!({
            "process": {"pid": pid},
            "signal": "SIGNAL_SIGKILL"
        })
    }

    async fn unary(
        &self,
        target: &TerminalTarget,
        method: &str,
        payload: serde_json::Value,
    ) -> anyhow::Result<()> {
        let resp = self
            .client
            .post(format!("{}/{}", target.envd_base_url(), method))
            .headers(unary_headers(target)?)
            .json(&payload)
            .timeout(Duration::from_secs(10))
            .send()
            .await
            .with_context(|| format!("failed to call envd {}", method))?;
        if !resp.status().is_success() {
            return Err(anyhow!(
                "envd {} failed with HTTP {}",
                method,
                resp.status()
            ));
        }
        Ok(())
    }
}

fn streaming_headers(target: &TerminalTarget) -> anyhow::Result<HeaderMap> {
    let mut headers = HeaderMap::new();
    headers.insert(
        "content-type",
        HeaderValue::from_static(CONNECT_CONTENT_TYPE),
    );
    headers.insert(
        "connect-protocol-version",
        HeaderValue::from_static(CONNECT_PROTOCOL_VERSION),
    );
    headers.insert(
        "connect-content-encoding",
        HeaderValue::from_static("identity"),
    );
    headers.insert("x-envd-user", HeaderValue::from_static(DEFAULT_USER));
    apply_target_headers(&mut headers, target)?;
    Ok(headers)
}

fn unary_headers(target: &TerminalTarget) -> anyhow::Result<HeaderMap> {
    let mut headers = HeaderMap::new();
    headers.insert("content-type", HeaderValue::from_static("application/json"));
    headers.insert(
        "connect-protocol-version",
        HeaderValue::from_static(CONNECT_PROTOCOL_VERSION),
    );
    headers.insert("x-envd-user", HeaderValue::from_static(DEFAULT_USER));
    apply_target_headers(&mut headers, target)?;
    Ok(headers)
}

fn apply_target_headers(headers: &mut HeaderMap, target: &TerminalTarget) -> anyhow::Result<()> {
    if let Some(token) = target
        .envd_access_token
        .as_deref()
        .filter(|v| !v.is_empty())
    {
        headers.insert("x-access-token", HeaderValue::from_str(token)?);
    }
    if let Some(container) = target.container_id.as_deref().filter(|v| !v.is_empty()) {
        headers.insert("x-cube-container-id", HeaderValue::from_str(container)?);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        body::Bytes, extract::State, http::HeaderMap as AxumHeaderMap, routing::post, Router,
    };
    use std::sync::Arc;
    use tokio::sync::Mutex;

    #[derive(Clone, Default)]
    struct EnvDCapture {
        start_headers: Arc<Mutex<Option<AxumHeaderMap>>>,
        start_body: Arc<Mutex<Option<Vec<u8>>>>,
        input_body: Arc<Mutex<Option<serde_json::Value>>>,
        resize_body: Arc<Mutex<Option<serde_json::Value>>>,
        signal_body: Arc<Mutex<Option<serde_json::Value>>>,
    }

    #[test]
    fn connect_envelope_round_trips_json_frame() {
        let body = br#"{"event":{"start":{"pid":42}}}"#;
        let frame = encode_connect_envelope(body);
        assert_eq!(frame[0], 0);
        assert_eq!(
            u32::from_be_bytes([frame[1], frame[2], frame[3], frame[4]]),
            body.len() as u32
        );

        let mut decoder = ConnectJsonDecoder::new();
        let messages = decoder.push(&frame).expect("frame should decode");
        assert_eq!(messages.len(), 1);
        assert_eq!(messages[0]["event"]["start"]["pid"], 42);
    }

    #[test]
    fn connect_decoder_buffers_partial_frames() {
        let frame = encode_connect_envelope(br#"{"event":{"data":{"pty":"aGk="}}}"#);
        let mut decoder = ConnectJsonDecoder::new();
        assert!(decoder.push(&frame[..3]).unwrap().is_empty());
        let messages = decoder.push(&frame[3..]).unwrap();
        assert_eq!(messages.len(), 1);
    }

    #[test]
    fn parse_pty_output_decodes_base64() {
        let event = serde_json::json!({"event": {"data": {"pty": "bHMNCg=="}}});
        assert_eq!(
            parse_pty_event(event).unwrap(),
            PtyEvent::Output {
                data: b"ls\r\n".to_vec()
            }
        );
    }

    #[test]
    fn terminal_payload_helpers_encode_input_resize_and_kill() {
        let input = TerminalService::input_payload(42, b"echo hi\n");
        assert_eq!(input["process"]["pid"], serde_json::json!(42));
        assert_eq!(input["input"]["pty"], serde_json::json!("ZWNobyBoaQo="));

        let resize = TerminalService::resize_payload(42, 0, 120);
        assert_eq!(resize["process"]["pid"], serde_json::json!(42));
        assert_eq!(resize["pty"]["size"]["rows"], serde_json::json!(1));
        assert_eq!(resize["pty"]["size"]["cols"], serde_json::json!(120));

        let kill = TerminalService::kill_payload(42);
        assert_eq!(kill["process"]["pid"], serde_json::json!(42));
        assert_eq!(kill["signal"], serde_json::json!("SIGNAL_SIGKILL"));
    }

    #[test]
    fn target_headers_include_access_token_and_container_id() {
        let target = TerminalTarget {
            sandbox_id: "sbx".to_string(),
            domain: "cube.app".to_string(),
            envd_access_token: Some("envd-token".to_string()),
            container_id: Some("container-1".to_string()),
            process_base_url: None,
        };

        let headers = streaming_headers(&target).expect("headers");
        assert_eq!(headers["x-access-token"], "envd-token");
        assert_eq!(headers["x-cube-container-id"], "container-1");
        assert_eq!(headers["x-envd-user"], DEFAULT_USER);
    }

    #[test]
    fn connect_decoders_keep_concurrent_session_streams_isolated() {
        let first = encode_connect_envelope(br#"{"event":{"data":{"pty":"Zmlyc3Q="}}}"#);
        let second = encode_connect_envelope(br#"{"event":{"data":{"pty":"c2Vjb25k"}}}"#);
        let mut decoder_a = ConnectJsonDecoder::new();
        let mut decoder_b = ConnectJsonDecoder::new();

        assert!(decoder_a.push(&first[..6]).unwrap().is_empty());
        let b_messages = decoder_b.push(&second).unwrap();
        assert_eq!(
            parse_pty_event(b_messages.into_iter().next().unwrap()).unwrap(),
            PtyEvent::Output {
                data: b"second".to_vec()
            }
        );

        let a_messages = decoder_a.push(&first[6..]).unwrap();
        assert_eq!(
            parse_pty_event(a_messages.into_iter().next().unwrap()).unwrap(),
            PtyEvent::Output {
                data: b"first".to_vec()
            }
        );
    }

    #[test]
    fn envd_base_url_uses_virtual_sandbox_host() {
        let target = TerminalTarget {
            sandbox_id: "sbx".to_string(),
            domain: "cube.app".to_string(),
            envd_access_token: None,
            container_id: None,
            process_base_url: None,
        };
        assert_eq!(
            target.envd_base_url(),
            "http://49983-sbx.cube.app/process.Process"
        );
    }

    #[tokio::test]
    async fn terminal_service_calls_envd_pty_process_api() {
        async fn start_handler(
            State(capture): State<EnvDCapture>,
            headers: AxumHeaderMap,
            body: Bytes,
        ) -> Vec<u8> {
            *capture.start_headers.lock().await = Some(headers);
            *capture.start_body.lock().await = Some(body.to_vec());
            encode_connect_envelope(br#"{"event":{"start":{"pid":321}}}"#)
        }

        async fn input_handler(
            State(capture): State<EnvDCapture>,
            axum::Json(body): axum::Json<serde_json::Value>,
        ) {
            *capture.input_body.lock().await = Some(body);
        }

        async fn resize_handler(
            State(capture): State<EnvDCapture>,
            axum::Json(body): axum::Json<serde_json::Value>,
        ) {
            *capture.resize_body.lock().await = Some(body);
        }

        async fn signal_handler(
            State(capture): State<EnvDCapture>,
            axum::Json(body): axum::Json<serde_json::Value>,
        ) {
            *capture.signal_body.lock().await = Some(body);
        }

        async fn spawn_server(app: Router) -> String {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("listener should bind");
            let addr = listener.local_addr().expect("listener addr");
            tokio::spawn(async move {
                axum::serve(listener, app).await.expect("server should run");
            });
            format!("http://{}", addr)
        }

        let capture = EnvDCapture::default();
        let base_url = spawn_server(
            Router::new()
                .route("/process.Process/Start", post(start_handler))
                .route("/process.Process/SendInput", post(input_handler))
                .route("/process.Process/Update", post(resize_handler))
                .route("/process.Process/SendSignal", post(signal_handler))
                .with_state(capture.clone()),
        )
        .await;

        let target = TerminalTarget {
            sandbox_id: "sbx".to_string(),
            domain: "cube.app".to_string(),
            envd_access_token: Some("token-1".to_string()),
            container_id: Some("container-1".to_string()),
            process_base_url: Some(format!("{}/process.Process", base_url)),
        };
        let service = TerminalService::new(reqwest::Client::new());

        let mut response = service
            .start(&target, 25, 100)
            .await
            .expect("PTY start should succeed");
        let start_chunk = response
            .chunk()
            .await
            .expect("chunk should read")
            .expect("start chunk should exist");
        let mut decoder = ConnectJsonDecoder::new();
        let messages = decoder
            .push(&start_chunk)
            .expect("start frame should decode");
        assert_eq!(
            parse_pty_event(messages.into_iter().next().unwrap()).unwrap(),
            PtyEvent::Start { pid: 321 }
        );

        service
            .send_input(&target, 321, b"ls\n")
            .await
            .expect("input should send");
        service
            .resize(&target, 321, 40, 120)
            .await
            .expect("resize should send");
        service.kill(&target, 321).await.expect("kill should send");

        let start_headers = capture
            .start_headers
            .lock()
            .await
            .clone()
            .expect("start headers");
        assert_eq!(start_headers["x-access-token"], "token-1");
        assert_eq!(start_headers["x-cube-container-id"], "container-1");

        let start_body = capture.start_body.lock().await.clone().expect("start body");
        let mut start_decoder = ConnectJsonDecoder::new();
        let start_payload = start_decoder
            .push(&start_body)
            .expect("start request should decode");
        assert_eq!(
            start_payload[0]["pty"]["size"]["rows"],
            serde_json::json!(25)
        );
        assert_eq!(
            start_payload[0]["pty"]["size"]["cols"],
            serde_json::json!(100)
        );

        assert_eq!(
            capture.input_body.lock().await.clone().expect("input body"),
            TerminalService::input_payload(321, b"ls\n")
        );
        assert_eq!(
            capture
                .resize_body
                .lock()
                .await
                .clone()
                .expect("resize body"),
            TerminalService::resize_payload(321, 40, 120)
        );
        assert_eq!(
            capture
                .signal_body
                .lock()
                .await
                .clone()
                .expect("signal body"),
            TerminalService::kill_payload(321)
        );
    }
}

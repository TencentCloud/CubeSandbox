// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

//! Validate the envelope rules that the transport library leaves to callers.
//! Message bytes, decompression, and generated protobuf decoding stay in the library.
use axum::{
    body::{Body, Bytes},
    response::{IntoResponse, Response},
};
use bytes::BytesMut;
use connectrpc::{envelope::Envelope, router::MethodKind, ConnectError, ErrorCode};
use futures::StreamExt;
use http::Request;
use std::sync::{Arc, Mutex};

#[derive(Clone, Default)]
pub(super) struct FrameError(Arc<Mutex<Option<ConnectError>>>);
impl FrameError {
    pub(super) fn get(&self) -> Option<ConnectError> {
        self.0.lock().expect("frame error").clone()
    }
    fn set(&self, error: ConnectError) {
        *self.0.lock().expect("frame error") = Some(error);
    }
}

fn incomplete(bytes: &[u8]) -> ConnectError {
    let message = if bytes.len() < 5 {
        "protocol error: incomplete envelope: unexpected EOF".to_owned()
    } else {
        let length = u32::from_be_bytes(bytes[1..5].try_into().unwrap());
        format!(
            "protocol error: promised {length} bytes in enveloped message, got {} bytes",
            bytes.len() - 5
        )
    };
    ConnectError::invalid_argument(message)
}
fn normalize(
    mut envelope: Envelope,
    end_flag: u8,
    compressed: bool,
    json: bool,
    extra: usize,
) -> Result<(Envelope, bool), ConnectError> {
    if envelope.flags & 1 != 0 && !compressed {
        return Err(ConnectError::internal(
            "protocol error: sent compressed message without compression support",
        ));
    }
    let special = envelope.flags & !1 != 0;
    if special {
        if envelope.flags & 1 != 0 && !envelope.data.is_empty() {
            envelope.data = decompress(envelope.data)?;
        }
        if extra > 0 {
            return Err(ConnectError::internal(format!(
                "corrupt response: {extra} extra bytes after end of stream"
            )));
        }
        if envelope.flags & end_flag == 0 {
            return Err(ConnectError::internal(format!(
                "protocol error: invalid envelope flags {}",
                envelope.flags
            )));
        }
        if end_flag == 2 {
            super::end_message::validate(&envelope.data)?;
        } else {
            for line in envelope.data.split(|byte| *byte == b'\n') {
                if line.is_empty() || line == b"\r" {
                    continue;
                }
                let Some(colon) = line.iter().position(|byte| *byte == b':') else {
                    return Err(ConnectError::internal(format!("gRPC-Web protocol error: trailers invalid: malformed MIME header: missing colon: {:?}", String::from_utf8_lossy(line.strip_suffix(b"\r").unwrap_or(line)))));
                };
                if http::HeaderName::from_bytes(&line[..colon]).is_err() {
                    return Err(ConnectError::internal(
                        "gRPC-Web protocol error: trailers invalid: malformed MIME header line",
                    ));
                }
            }
        }
        // The client-stream decoder uses the Connect end marker for every protocol.
        envelope.flags = 2;
        envelope.data = Bytes::from_static(b"{}");
    } else if json && envelope.data.is_empty() {
        // Go treats a zero-length framed message as the protobuf zero value,
        // including JSON and an empty frame with its compression bit set.
        envelope.flags = 0;
        envelope.data = Bytes::from_static(b"{}");
    }
    Ok((envelope, special))
}

fn decompress(bytes: Bytes) -> Result<Bytes, ConnectError> {
    connectrpc::CompressionRegistry::default()
        .decompress_with_limit("gzip", bytes, usize::MAX)
        .map_err(|mut error| {
            if error.message.as_deref().is_some_and(|message| {
                matches!(
                    message,
                    "gzip data too short for header" | "gzip header truncated"
                )
            }) {
                error.message = Some("get decompressor: unexpected EOF".into());
            }
            error
        })
}
fn validate_message(path: &str, wire: &Bytes, json: bool) -> Result<(), ConnectError> {
    let mut bytes = BytesMut::from(wire.as_ref());
    let mut envelope = Envelope::decode(&mut bytes)
        .expect("complete envelope")
        .expect("complete envelope");
    if envelope.is_compressed() {
        envelope.data = decompress(envelope.data)?;
    }
    let format = if json {
        connectrpc::CodecFormat::Json
    } else {
        connectrpc::CodecFormat::Proto
    };
    super::json::message(path, connectrpc::Payload::new(envelope.data, format))?;
    Ok(())
}

pub(super) fn error_response(media: &str, error: &ConnectError) -> Response {
    if media.starts_with("application/grpc") {
        let mut response = (
            [
                ("content-type", media.to_owned()),
                ("grpc-status", error.code.grpc_code().to_string()),
                (
                    "grpc-message",
                    percent_message(error.message.as_deref().unwrap_or("")),
                ),
                ("grpc-accept-encoding", "gzip".into()),
            ],
            Body::empty(),
        )
            .into_response();
        if !media.starts_with("application/grpc-web") {
            let mut trailers = http::HeaderMap::new();
            for key in ["grpc-status", "grpc-message"] {
                if let Some(value) = response.headers_mut().remove(key) {
                    trailers.insert(key, value);
                }
            }
            response.headers_mut().insert(
                http::header::TRAILER,
                http::HeaderValue::from_static("grpc-status, grpc-message"),
            );
            *response.body_mut() = super::grpc::trailers_body(trailers);
        }
        return response;
    }
    let data = serde_json::to_vec(&serde_json::json!({"error": {"code": error.code.as_str(), "message": error.message.as_deref().unwrap_or("")}})).expect("error JSON");
    let mut wire = vec![2];
    wire.extend_from_slice(&(data.len() as u32).to_be_bytes());
    wire.extend_from_slice(&data);
    (
        [
            ("content-type", media.to_owned()),
            ("connect-accept-encoding", "gzip".into()),
        ],
        wire,
    )
        .into_response()
}
fn percent_message(value: &str) -> String {
    use std::fmt::Write;
    let mut result = String::new();
    for byte in value.bytes() {
        if (0x20..0x7f).contains(&byte) && byte != b'%' {
            result.push(byte as char);
        } else {
            write!(result, "%{byte:02X}").expect("string write");
        }
    }
    result
}

pub(super) async fn request(
    kind: MethodKind,
    mut request: Request<Body>,
) -> Result<Request<Body>, Response> {
    let media = request
        .headers()
        .get("content-type")
        .and_then(|h| h.to_str().ok())
        .unwrap_or("")
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .to_owned();
    let connect = media.starts_with("application/connect+");
    if !connect && !media.starts_with("application/grpc") {
        return Ok(request);
    }
    if !connect {
        if let Some(raw) = request
            .headers()
            .get("grpc-timeout")
            .and_then(|h| h.to_str().ok())
        {
            let timeout =
                super::grpc::timeout(raw).map_err(|error| error_response(&media, &error))?;
            if timeout.is_some_and(|timeout| timeout.is_zero())
                && request.uri().path() != "/process.Process/Connect"
            {
                return Err(error_response(
                    &media,
                    &ConnectError::deadline_exceeded("context deadline exceeded"),
                ));
            }
        }
    }
    if !connect
        && matches!(
            request.uri().path(),
            "/process.Process/SendInput"
                | "/process.Process/StreamInput"
                | "/process.Process/CloseStdin"
        )
    {
        // The Go handlers serialize accepted writes independently of request cancellation.
        request.headers_mut().remove("grpc-timeout");
    }
    let encoding = request
        .headers()
        .get(if connect {
            "connect-content-encoding"
        } else {
            "grpc-encoding"
        })
        .and_then(|h| h.to_str().ok())
        .unwrap_or("");
    // Compression negotiation precedes reading the body in the upstream handler.
    if !matches!(encoding, "" | "identity" | "gzip") {
        return Ok(request);
    }
    let compressed = encoding == "gzip";
    let json = media.ends_with("json");
    let end_flag = if connect {
        2
    } else if media.starts_with("application/grpc-web") {
        128
    } else {
        0
    };
    let path = request.uri().path().to_owned();
    let (mut parts, body) = request.into_parts();
    if matches!(
        kind,
        MethodKind::ClientStreaming | MethodKind::BidiStreaming
    ) {
        let error = FrameError::default();
        parts.extensions.insert(error.clone());
        let stream = futures::stream::unfold(
            (body.into_data_stream(), BytesMut::new(), false, error),
            move |(mut body, mut pending, mut done, error)| async move {
                if done {
                    return None;
                }
                loop {
                    // Validate before passing the complete message to the generated decoder.
                    if let Some(envelope) =
                        Envelope::decode(&mut pending).expect("unlimited envelope")
                    {
                        let mut extra = pending.len();
                        if envelope.flags & !1 != 0 {
                            while let Some(chunk) = body.next().await {
                                match chunk {
                                    Ok(bytes) => extra = extra.saturating_add(bytes.len()),
                                    Err(_) => {
                                        let failure = ConnectError::canceled("context canceled");
                                        error.set(failure.clone());
                                        return Some((Err(failure), (body, pending, true, error)));
                                    }
                                }
                            }
                        }
                        match normalize(envelope, end_flag, compressed, json, extra) {
                            Ok((envelope, ended)) => {
                                done = ended;
                                return Some((Ok(envelope.encode()), (body, pending, done, error)));
                            }
                            Err(failure) => {
                                error.set(failure.clone());
                                return Some((Err(failure), (body, pending, true, error)));
                            }
                        }
                    }
                    match body.next().await {
                        Some(Ok(bytes)) => pending.extend_from_slice(&bytes),
                        Some(Err(_)) => {
                            let failure = ConnectError::canceled("context canceled");
                            error.set(failure.clone());
                            return Some((Err(failure), (body, pending, true, error)));
                        }
                        None if pending.is_empty() => return None,
                        None => {
                            let failure = incomplete(&pending);
                            error.set(failure.clone());
                            return Some((Err(failure), (body, pending, true, error)));
                        }
                    }
                }
            },
        );
        return Ok(Request::from_parts(parts, Body::from_stream(stream)));
    }
    let bytes = axum::body::to_bytes(body, usize::MAX)
        .await
        .map_err(|_| error_response(&media, &ConnectError::canceled("context canceled")))?;
    let mut remaining = BytesMut::from(bytes.as_ref());
    let mut message: Option<Bytes> = None;
    loop {
        if remaining.is_empty() {
            break;
        }
        if let Some(first) = &message {
            validate_message(&path, first, json).map_err(|error| error_response(&media, &error))?;
        }
        if remaining.len() < 5 {
            return Err(error_response(&media, &incomplete(&remaining)));
        }
        let envelope = Envelope::decode(&mut remaining)
            .expect("unlimited envelope")
            .ok_or_else(|| error_response(&media, &incomplete(&remaining)))?;
        let (envelope, ended) = normalize(envelope, end_flag, compressed, json, remaining.len())
            .map_err(|error| error_response(&media, &error))?;
        if ended {
            break;
        }
        if message.is_some() {
            validate_message(&path, &envelope.encode(), json)
                .map_err(|error| error_response(&media, &error))?;
            return Err(error_response(
                &media,
                &ConnectError::new(
                    ErrorCode::Unimplemented,
                    "unary request has multiple messages",
                ),
            ));
        }
        message = Some(envelope.encode());
    }
    let message = message.ok_or_else(|| {
        error_response(
            &media,
            &ConnectError::new(ErrorCode::Unimplemented, "unary request has zero messages"),
        )
    })?;
    parts.headers.remove(http::header::CONTENT_LENGTH);
    Ok(Request::from_parts(parts, Body::from(message)))
}

pub(super) fn compressed_response(response: Response) -> Response {
    let (mut parts, body) = response.into_parts();
    parts.headers.insert(
        "connect-content-encoding",
        http::HeaderValue::from_static("gzip"),
    );
    parts.headers.remove(http::header::CONTENT_LENGTH);
    let stream = futures::stream::unfold(
        (body.into_data_stream(), BytesMut::new()),
        |(mut body, mut pending)| async move {
            loop {
                if let Some(mut envelope) =
                    Envelope::decode(&mut pending).expect("response envelope")
                {
                    if envelope.flags & 1 == 0 && !envelope.data.is_empty() {
                        envelope.data = connectrpc::CompressionRegistry::default()
                            .compress("gzip", &envelope.data)
                            .expect("gzip provider");
                        envelope.flags |= 1;
                    }
                    return Some((Ok(envelope.encode()), (body, pending)));
                }
                match body.next().await {
                    Some(Ok(bytes)) => pending.extend_from_slice(&bytes),
                    Some(Err(error)) => return Some((Err(error), (body, pending))),
                    None => return None,
                }
            }
        },
    );
    Response::from_parts(parts, Body::from_stream(stream))
}

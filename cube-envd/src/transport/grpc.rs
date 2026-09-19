// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use axum::{
    body::{Body, Bytes},
    response::Response,
};
use bytes::BytesMut;
use connectrpc::{envelope::Envelope, ConnectError};
use futures::StreamExt;
use http::{HeaderMap, HeaderName, HeaderValue};
use http_body::Frame;
use http_body_util::{BodyExt, StreamBody};

pub(super) fn timeout(raw: &str) -> Result<Option<std::time::Duration>, ConnectError> {
    if raw.is_empty() {
        return Ok(None);
    }
    let unit = *raw.as_bytes().last().unwrap();
    let scale: i64 = match unit {
        b'n' => 1,
        b'u' => 1000,
        b'm' => 1_000_000,
        b'S' => 1_000_000_000,
        b'M' => 60_000_000_000,
        b'H' => 3_600_000_000_000,
        _ => {
            return Err(ConnectError::invalid_argument(format!(
                "protocol error: timeout has invalid unit {:?}",
                unit as char
            )))
        }
    };
    let value = raw[..raw.len() - 1]
        .parse::<i64>()
        .ok()
        .filter(|value| *value >= 0)
        .ok_or_else(|| {
            ConnectError::invalid_argument(format!("protocol error: invalid timeout {raw:?}"))
        })?;
    if value > 99_999_999 {
        return Err(ConnectError::invalid_argument(format!(
            "protocol error: timeout {raw:?} is too long"
        )));
    }
    Ok(value
        .checked_mul(scale)
        .map(|nanos| std::time::Duration::from_nanos(nanos as u64)))
}

pub(super) fn trailers_body(trailers: HeaderMap) -> Body {
    Body::new(StreamBody::new(futures::stream::once(async move {
        Ok::<_, std::convert::Infallible>(Frame::<Bytes>::trailers(trailers))
    })))
}

fn clean_trailers(bytes: &[u8]) -> Bytes {
    let mut result = Vec::new();
    for line in bytes.split(|byte| *byte == b'\n') {
        if line.is_empty() || line.starts_with(b"grpc-status-details-bin:") {
            continue;
        }
        result.extend_from_slice(line);
        result.push(b'\n');
    }
    result.into()
}

pub(super) async fn response(response: Response, web: bool) -> Response {
    let (mut parts, body) = response.into_parts();
    parts.headers.remove("grpc-status-details-bin");
    if !web {
        parts.headers.insert(
            http::header::TRAILER,
            HeaderValue::from_static("grpc-status, grpc-message"),
        );
        if let Some(status) = parts.headers.remove("grpc-status") {
            let mut trailers = HeaderMap::new();
            trailers.insert("grpc-status", status);
            if let Some(message) = parts.headers.remove("grpc-message") {
                trailers.insert("grpc-message", message);
            }
            parts.headers.remove(http::header::CONTENT_LENGTH);
            return Response::from_parts(parts, trailers_body(trailers));
        }
        return Response::from_parts(
            parts,
            Body::new(body.map_frame(|frame| match frame.into_trailers() {
                Ok(mut trailers) => {
                    trailers.remove("grpc-status-details-bin");
                    Frame::trailers(trailers)
                }
                Err(frame) => frame,
            })),
        );
    }
    if parts.headers.contains_key("grpc-status") {
        return Response::from_parts(parts, body);
    }
    let compressed = parts
        .headers
        .get("grpc-encoding")
        .is_some_and(|value| value == "gzip");
    let mut body = body.into_data_stream();
    let mut pending = BytesMut::new();
    let first = loop {
        if let Some(envelope) = Envelope::decode(&mut pending).expect("response envelope") {
            break envelope;
        }
        match body.next().await {
            Some(Ok(bytes)) => pending.extend_from_slice(&bytes),
            Some(Err(error)) => {
                return Response::from_parts(
                    parts,
                    Body::from_stream(futures::stream::once(async { Err::<Bytes, _>(error) })),
                )
            }
            None => return Response::from_parts(parts, Body::empty()),
        }
    };
    if first.flags == 0x80 {
        for line in clean_trailers(&first.data).split(|byte| *byte == b'\n') {
            let Some(colon) = line.iter().position(|byte| *byte == b':') else {
                continue;
            };
            let Ok(key) = HeaderName::from_bytes(&line[..colon]) else {
                continue;
            };
            let value = line[colon + 1..]
                .strip_prefix(b" ")
                .unwrap_or(&line[colon + 1..]);
            let value = value.strip_suffix(b"\r").unwrap_or(value);
            if let Ok(value) = HeaderValue::from_bytes(value) {
                parts.headers.insert(key, value);
            }
        }
        parts.headers.remove(http::header::CONTENT_LENGTH);
        return Response::from_parts(parts, Body::empty());
    }
    let output = futures::stream::unfold(
        (body, pending, Some(first)),
        move |(mut body, mut pending, mut first)| async move {
            loop {
                let envelope = first
                    .take()
                    .or_else(|| Envelope::decode(&mut pending).expect("response envelope"));
                if let Some(mut envelope) = envelope {
                    if envelope.flags == 0x80 {
                        envelope.data = clean_trailers(&envelope.data);
                        if compressed {
                            use std::io::Write;
                            let mut encoder = flate2::write::GzEncoder::new(
                                Vec::new(),
                                flate2::Compression::default(),
                            );
                            encoder.write_all(&envelope.data).expect("gzip Vec write");
                            envelope.data = encoder.finish().expect("gzip Vec finish").into();
                            envelope.flags |= 1;
                        }
                    }
                    return Some((Ok(envelope.encode()), (body, pending, None)));
                }
                match body.next().await {
                    Some(Ok(bytes)) => pending.extend_from_slice(&bytes),
                    Some(Err(error)) => return Some((Err(error), (body, pending, None))),
                    None => return None,
                }
            }
        },
    );
    parts.headers.remove(http::header::CONTENT_LENGTH);
    Response::from_parts(parts, Body::from_stream(output))
}

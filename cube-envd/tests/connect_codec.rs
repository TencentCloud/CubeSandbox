// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::convert::Infallible;

use axum::body::Body;
use bytes::Bytes;
use cube_envd::connect::{
    decode_frame, decode_request_frame, encode_frame, ConnectError, RequestFrameReader,
    END_STREAM_FLAG, MAX_FRAME_BYTES, MAX_UNARY_JSON_BYTES,
};
use futures_util::stream;

// 验证 Connect JSON 帧编码后可无损解码。
#[test]
fn round_trips_a_connect_json_frame() {
    let encoded = encode_frame(0, br#"{"event":{"start":{"pid":7}}}"#).unwrap();

    let frame = decode_frame(&encoded).unwrap();

    assert_eq!(frame.flags, 0);
    assert_eq!(frame.payload, br#"{"event":{"start":{"pid":7}}}"#);
}

// 验证帧长度在分配载荷缓冲区前受到协议上限保护。
#[test]
fn rejects_a_frame_larger_than_the_protocol_limit_before_allocating() {
    let mut encoded = vec![0; 5];
    encoded[1..].copy_from_slice(&(MAX_FRAME_BYTES as u32 + 1).to_be_bytes());

    let error = decode_frame(&encoded).unwrap_err();

    assert!(matches!(error, ConnectError::ResourceExhausted { .. }));
}

// 钉住协议上限的取值依据：daemon 侧必须与 CubeSandbox 自带 SDK 的接收上限一致，
// 否则会出现"SDK 允许发、daemon 拒绝收"的不对称。
//
// 对端常量（三处必须同时成立才能改这里）：
//   sdk/go/connect.go:20                      maxConnectEnvelopeSize
//   sdk/node/src/commands.ts:14               MAX_CONNECT_ENVELOPE_SIZE
//   sdk/python/cubesandbox/_commands.py:24    MAX_CONNECT_ENVELOPE_SIZE
#[test]
fn protocol_limits_match_the_sdk_envelope_size() {
    const SDK_MAX_CONNECT_ENVELOPE_SIZE: usize = 64 * 1024 * 1024;

    assert_eq!(MAX_FRAME_BYTES, SDK_MAX_CONNECT_ENVELOPE_SIZE);
    assert_eq!(MAX_UNARY_JSON_BYTES, 4 * 1024 * 1024);
}

// 验证流结束标志会在帧编解码中保留。
#[test]
fn end_stream_flag_is_preserved() {
    let encoded = encode_frame(END_STREAM_FLAG, b"{}").unwrap();

    assert_eq!(decode_frame(&encoded).unwrap().flags, END_STREAM_FLAG);
}

// 验证一元请求即使第二帧位于后续数据块也会被拒绝。
#[tokio::test]
async fn rejects_a_second_frame_arriving_in_a_later_body_chunk() {
    let first = encode_frame(0, br#"{}"#).unwrap();
    let second = encode_frame(0, br#"{}"#).unwrap();
    let body = Body::from_stream(stream::iter([
        Ok::<_, Infallible>(Bytes::from(first)),
        Ok(Bytes::from(second)),
    ]));

    assert!(decode_request_frame(body).await.is_err());
}

// 验证流式读取器按顺序一次读取一帧并在末尾返回 None。
#[tokio::test]
async fn reads_streaming_request_frames_one_at_a_time() {
    let first = encode_frame(0, br#"{"start":{}}"#).unwrap();
    let second = encode_frame(0, br#"{"data":{}}"#).unwrap();
    let body = Body::from_stream(stream::iter([
        Ok::<_, Infallible>(Bytes::from(first)),
        Ok(Bytes::from(second)),
    ]));
    let mut reader = RequestFrameReader::new(body);

    assert_eq!(
        reader.next_frame().await.unwrap().unwrap().payload,
        br#"{"start":{}}"#
    );
    assert_eq!(
        reader.next_frame().await.unwrap().unwrap().payload,
        br#"{"data":{}}"#
    );
    assert!(reader.next_frame().await.unwrap().is_none());
}

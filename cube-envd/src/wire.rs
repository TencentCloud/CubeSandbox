// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use serde::{de::DeserializeOwned, Serialize};

/// 按 protobuf JSON 规则反序列化由生成类型表示的协议消息。
pub fn decode_json<T>(payload: &[u8], message_name: &str) -> Result<T, crate::connect::RpcError>
where
    T: DeserializeOwned,
{
    serde_json::from_slice(payload).map_err(|error| {
        crate::connect::RpcError::invalid_argument(format!("invalid {message_name}: {error}"))
    })
}

/// 按 protobuf JSON 规则序列化由生成类型表示的协议消息。
pub fn encode_json<T>(message: &T) -> Result<Vec<u8>, crate::connect::RpcError>
where
    T: Serialize,
{
    serde_json::to_vec(message).map_err(|error| {
        crate::connect::RpcError::new(
            crate::connect::Code::Internal,
            format!("serialize protobuf JSON: {error}"),
        )
    })
}

/// 需要按 protobuf JSON 规范归一化的时间戳字段名。
///
/// 规范要求 `google.protobuf.Timestamp` 的输出"始终 Z 归一化"
/// （protobuf.dev/programming-guides/json）。`pbjson-types` 用 `time` 的 RFC3339
/// 格式化，UTC 会写成 `+00:00`；参考实现（protobuf-go）写 `Z`。两种写法在语义上
/// 等价，但属于逐字节可观察的协议输出，且 SDK 可能对字符串做比较/落库。
const TIMESTAMP_FIELDS: [&str; 1] = ["modifiedTime"];

/// 把已知时间戳字段的 `+00:00` 归一成 `Z`。
fn z_normalize_timestamps(value: &mut serde_json::Value) {
    match value {
        serde_json::Value::Object(fields) => {
            for (key, entry) in fields.iter_mut() {
                if TIMESTAMP_FIELDS.contains(&key.as_str()) {
                    if let serde_json::Value::String(text) = entry {
                        if let Some(prefix) = text.strip_suffix("+00:00") {
                            *text = format!("{prefix}Z");
                        }
                    }
                } else {
                    z_normalize_timestamps(entry);
                }
            }
        }
        serde_json::Value::Array(items) => {
            for item in items {
                z_normalize_timestamps(item);
            }
        }
        _ => {}
    }
}

/// 序列化协议消息为 JSON 取值，并做 protobuf JSON 规范化。
///
/// 与 `encode_json` 的差别是返回值可以继续被上层包装（例如放进流式帧），代价是
/// 多一次内存拷贝；需要逐字段规范化的响应（含时间戳的 filesystem 族）走这条路径。
pub fn encode_json_value<T>(message: &T) -> Result<serde_json::Value, crate::connect::RpcError>
where
    T: Serialize,
{
    let mut value = serde_json::to_value(message).map_err(|error| {
        crate::connect::RpcError::new(
            crate::connect::Code::Internal,
            format!("serialize protobuf JSON: {error}"),
        )
    })?;
    z_normalize_timestamps(&mut value);
    Ok(value)
}

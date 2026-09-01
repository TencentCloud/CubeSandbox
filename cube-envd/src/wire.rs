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

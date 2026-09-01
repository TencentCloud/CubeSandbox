use axum::{
    body::Body,
    http::{header::CONTENT_TYPE, HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use futures_util::StreamExt;
use serde::Serialize;
use std::time::Duration;
use thiserror::Error;

/// 标识 Connect 流结束消息的帧标志位。
pub const END_STREAM_FLAG: u8 = 0x02;
/// 限制单个 Connect 帧的最大载荷，避免无界内存分配。
pub const MAX_FRAME_BYTES: usize = 16 * 1024 * 1024;
/// 限制一元 JSON 请求体的最大大小。
pub const MAX_UNARY_JSON_BYTES: usize = 1024 * 1024;
/// 定义客户端未指定保活间隔时的默认值。
const DEFAULT_KEEPALIVE_INTERVAL: Duration = Duration::from_secs(90);

#[derive(Debug, PartialEq, Eq)]
/// 表示已拆解的 Connect 协议帧。
pub struct Frame {
    /// Connect 帧标志位。
    pub flags: u8,
    /// 帧中携带的原始消息载荷。
    pub payload: Vec<u8>,
}

/// 从分块 HTTP 请求体中顺序读取 Connect 帧。
pub struct RequestFrameReader {
    /// 尚未消费完的 HTTP 数据块。
    stream: axum::body::BodyDataStream,
    /// 留待下一次读取的剩余数据块。
    pending: Option<bytes::Bytes>,
}

/// 提供请求帧读取器的构造和增量读取操作。
impl RequestFrameReader {
    /// 用 HTTP 请求体创建增量帧读取器。
    pub fn new(body: Body) -> Self {
        Self {
            stream: body.into_data_stream(),
            pending: None,
        }
    }

    /// 读取下一帧，并在请求体结束时返回 None。
    pub async fn next_frame(&mut self) -> Result<Option<Frame>, RpcError> {
        let mut header = [0_u8; 5];
        let header_bytes = self.read_exact(&mut header).await?;
        if header_bytes == 0 {
            return Ok(None);
        }
        if header_bytes != header.len() {
            return Err(RpcError::invalid_argument(
                "incomplete Connect request frame",
            ));
        }

        let payload_len =
            u32::from_be_bytes(header[1..].try_into().expect("frame header")) as usize;
        if payload_len > MAX_FRAME_BYTES {
            return Err(RpcError::new(
                Code::ResourceExhausted,
                "Connect request frame exceeds 16 MiB",
            ));
        }
        let mut payload = vec![0_u8; payload_len];
        if self.read_exact(&mut payload).await? != payload_len {
            return Err(RpcError::invalid_argument(
                "incomplete Connect request frame",
            ));
        }

        validate_request_frame(Frame {
            flags: header[0],
            payload,
        })
        .map(Some)
    }

    /// 从分块请求体中尽可能填满目标缓冲区。
    async fn read_exact(&mut self, destination: &mut [u8]) -> Result<usize, RpcError> {
        let mut copied = 0;
        while copied < destination.len() {
            if self.pending.is_none() {
                match self.stream.next().await {
                    Some(Ok(chunk)) if !chunk.is_empty() => self.pending = Some(chunk),
                    Some(Ok(_)) => continue,
                    Some(Err(error)) => {
                        return Err(RpcError::new(Code::Internal, error.to_string()))
                    }
                    None => break,
                }
            }

            let pending = self.pending.take().expect("pending body chunk");
            let count = (destination.len() - copied).min(pending.len());
            destination[copied..copied + count].copy_from_slice(&pending[..count]);
            copied += count;
            if count < pending.len() {
                self.pending = Some(pending.slice(count..));
            }
        }
        Ok(copied)
    }
}

#[derive(Debug, Error, PartialEq, Eq)]
/// 描述独立 Connect 帧编解码失败的原因。
pub enum ConnectError {
    /// 帧头或载荷长度不完整。
    #[error("incomplete Connect frame")]
    IncompleteFrame,
    /// 帧声明或实际大小超过允许上限。
    #[error("Connect message size {size} exceeds maximum {max}")]
    ResourceExhausted { size: usize, max: usize },
}

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
/// 定义 Connect 错误响应使用的规范错误码。
pub enum Code {
    /// 请求格式或参数无效。
    InvalidArgument,
    /// 请求的资源不存在。
    NotFound,
    /// 请求认证失败。
    Unauthenticated,
    /// 服务尚未支持该调用。
    Unimplemented,
    /// 请求超过资源或容量限制。
    ResourceExhausted,
    /// 服务内部处理失败。
    Internal,
}

#[derive(Debug, Serialize)]
/// 表示返回给 Connect 客户端的 JSON 错误主体。
pub struct ErrorBody<'a> {
    /// 机器可读的错误码。
    pub code: Code,
    /// 面向调用方的错误说明。
    pub message: &'a str,
}

#[derive(Debug)]
/// 在路由处理过程中携带 Connect 错误码和消息。
pub struct RpcError {
    /// 要序列化给客户端的错误码。
    pub code: Code,
    /// 要序列化给客户端的错误消息。
    pub message: String,
    /// 可选的 HTTP 状态覆盖，用于媒体类型等传输层错误。
    status: Option<StatusCode>,
}

/// 提供常用 RPC 错误的构造函数。
impl RpcError {
    /// 使用指定错误码和消息创建 RPC 错误。
    pub fn new(code: Code, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            status: None,
        }
    }

    /// 创建参数无效错误。
    pub fn invalid_argument(message: impl Into<String>) -> Self {
        Self::new(Code::InvalidArgument, message)
    }

    /// 创建媒体类型不受支持错误，同时保留 Connect 参数错误码。
    pub fn unsupported_media_type(message: impl Into<String>) -> Self {
        Self {
            code: Code::InvalidArgument,
            message: message.into(),
            status: Some(StatusCode::UNSUPPORTED_MEDIA_TYPE),
        }
    }

    /// 创建尚未实现错误。
    pub fn unimplemented(message: impl Into<String>) -> Self {
        Self::new(Code::Unimplemented, message)
    }
}

/// 将内部 RPC 错误转换为 HTTP 和 Connect JSON 响应。
impl IntoResponse for RpcError {
    /// 根据错误码选择 HTTP 状态并序列化错误主体。
    fn into_response(self) -> Response {
        let status = self.status.unwrap_or_else(|| match self.code {
            Code::InvalidArgument => StatusCode::BAD_REQUEST,
            Code::NotFound => StatusCode::NOT_FOUND,
            Code::Unauthenticated => StatusCode::UNAUTHORIZED,
            Code::Unimplemented => StatusCode::NOT_IMPLEMENTED,
            Code::ResourceExhausted => StatusCode::TOO_MANY_REQUESTS,
            Code::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        });
        (
            status,
            Json(ErrorBody {
                code: self.code,
                message: &self.message,
            }),
        )
            .into_response()
    }
}

/// 校验一元 RPC 所需的 Content-Type 和 Connect 协议版本。
pub fn require_unary(headers: &HeaderMap) -> Result<(), RpcError> {
    let content_type = headers
        .get(CONTENT_TYPE)
        .and_then(|value| value.to_str().ok());
    if content_type != Some("application/json") {
        return Err(RpcError::unsupported_media_type(
            "unary RPCs require Content-Type: application/json",
        ));
    }
    if headers
        .get("Connect-Protocol-Version")
        .and_then(|value| value.to_str().ok())
        != Some("1")
    {
        return Err(RpcError::invalid_argument(
            "unary RPCs require Connect-Protocol-Version: 1",
        ));
    }

    Ok(())
}

/// 校验流式 RPC 的协议头并拒绝压缩请求。
pub fn require_streaming(headers: &HeaderMap) -> Result<(), RpcError> {
    let content_type = headers
        .get(CONTENT_TYPE)
        .and_then(|value| value.to_str().ok());
    if content_type != Some("application/connect+json") {
        return Err(RpcError::unsupported_media_type(
            "streaming RPCs require Content-Type: application/connect+json",
        ));
    }
    if headers
        .get("Connect-Protocol-Version")
        .and_then(|value| value.to_str().ok())
        != Some("1")
    {
        return Err(RpcError::invalid_argument(
            "streaming RPCs require Connect-Protocol-Version: 1",
        ));
    }
    if headers
        .get("Connect-Content-Encoding")
        .is_some_and(|value| value != "identity")
    {
        return Err(RpcError::unimplemented(
            "Connect compression is not supported",
        ));
    }

    Ok(())
}

/// 从请求头解析客户端保活间隔，并在缺失或非法时使用默认值。
pub fn keepalive_interval(headers: &HeaderMap) -> Duration {
    headers
        .get("Keepalive-Ping-Interval")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<u64>().ok())
        .filter(|seconds| *seconds > 0)
        .map(Duration::from_secs)
        .unwrap_or(DEFAULT_KEEPALIVE_INTERVAL)
}

/// 将标志和载荷编码为带五字节前缀的 Connect 帧。
pub fn encode_frame(flags: u8, payload: &[u8]) -> Result<Vec<u8>, ConnectError> {
    if payload.len() > MAX_FRAME_BYTES {
        return Err(ConnectError::ResourceExhausted {
            size: payload.len(),
            max: MAX_FRAME_BYTES,
        });
    }

    let mut encoded = Vec::with_capacity(5 + payload.len());
    encoded.push(flags);
    encoded.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    encoded.extend_from_slice(payload);
    Ok(encoded)
}

/// 校验并拆解一个完整的 Connect 帧缓冲区。
pub fn decode_frame(encoded: &[u8]) -> Result<Frame, ConnectError> {
    if encoded.len() < 5 {
        return Err(ConnectError::IncompleteFrame);
    }

    let size = u32::from_be_bytes(encoded[1..5].try_into().expect("five-byte prefix")) as usize;
    if size > MAX_FRAME_BYTES {
        return Err(ConnectError::ResourceExhausted {
            size,
            max: MAX_FRAME_BYTES,
        });
    }
    if encoded.len() != 5 + size {
        return Err(ConnectError::IncompleteFrame);
    }

    Ok(Frame {
        flags: encoded[0],
        payload: encoded[5..].to_vec(),
    })
}

/// 从 HTTP 请求体读取且只接受唯一一个 Connect 请求帧。
pub async fn decode_request_frame(body: Body) -> Result<Frame, RpcError> {
    let mut reader = RequestFrameReader::new(body);
    let frame = reader
        .next_frame()
        .await?
        .ok_or_else(|| RpcError::invalid_argument("incomplete Connect request frame"))?;
    if reader.next_frame().await?.is_some() {
        return Err(RpcError::invalid_argument(
            "Connect request must contain exactly one frame",
        ));
    }
    Ok(frame)
}

/// 拒绝当前服务不支持的压缩和非零请求帧标志。
fn validate_request_frame(frame: Frame) -> Result<Frame, RpcError> {
    if frame.flags & 0x01 != 0 {
        return Err(RpcError::unimplemented(
            "compressed Connect request frames are not supported",
        ));
    }
    if frame.flags != 0 {
        return Err(RpcError::invalid_argument(
            "Connect request frames must use flags 0",
        ));
    }

    Ok(frame)
}

/// 构造正常结束或携带错误的 Connect 流结束帧。
pub fn end_stream(error: Option<RpcError>) -> Vec<u8> {
    let payload = match error {
        Some(error) => serde_json::to_vec(&serde_json::json!({
            "error": { "code": error.code, "message": error.message }
        }))
        .expect("serialize Connect error"),
        None => b"{}".to_vec(),
    };
    encode_frame(END_STREAM_FLAG, &payload).expect("bounded Connect end stream")
}

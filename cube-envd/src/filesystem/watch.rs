use std::{
    future::Future,
    pin::Pin,
    sync::{Arc, Mutex as StdMutex},
};

use axum::{
    body::Body,
    extract::Request,
    response::{IntoResponse, Response},
};
use bytes::Bytes as StreamBytes;
use futures_util::stream::Stream;
use notify::{RecursiveMode, Watcher};
use tokio::{fs, sync::mpsc};

use crate::{
    auth::request_user,
    connect::{
        decode_request_frame, encode_frame, end_stream, keepalive_interval, require_streaming,
        Code, RpcError,
    },
    generated::filesystem as proto,
    wire,
};

use super::{
    entries::{
        entry_info_sync, is_network_mount, record_watch_failure, resolve, watch_event_kind,
    },
    error::filesystem_error,
    model::{proto_entry, watch_frame},
};

pub async fn watch_dir(
    axum::extract::State(state): axum::extract::State<crate::app::AppState>,
    request: Request,
) -> Result<Response, RpcError> {
    require_streaming(request.headers())?;
    let keepalive = keepalive_interval(request.headers());
    let user = request_user(request.headers())
        .map_err(|error| RpcError::new(Code::Unauthenticated, error.to_string()))?;
    let frame = decode_request_frame(request.into_body()).await?;
    if frame.flags != 0 {
        return Err(RpcError::invalid_argument(
            "WatchDir request frame must use flags 0",
        ));
    }
    let request: proto::WatchDirRequest = wire::decode_json(&frame.payload, "WatchDir request")?;
    let path = resolve(&request.path, &user)?;
    let metadata = fs::metadata(&path)
        .await
        .map_err(|error| filesystem_error(&path, error))?;
    if !metadata.is_dir() {
        return Err(RpcError::invalid_argument(format!(
            "path {} is not a directory",
            path.display()
        )));
    }
    if !request.allow_network_mounts && is_network_mount(&path).await? {
        return Err(RpcError::invalid_argument(format!(
            "cannot watch network filesystem path {}",
            path.display()
        )));
    }

    let (sender, receiver) = mpsc::channel(128);
    let failure = Arc::new(StdMutex::new(None));
    let callback_sender = sender.clone();
    let callback_failure = Arc::clone(&failure);
    let watch_root = path.clone();
    let include_entry = request.include_entry;
    let mut watcher = notify::recommended_watcher(move |result: notify::Result<notify::Event>| {
        let event = match result {
            Ok(event) => event,
            Err(error) => {
                let error = RpcError::new(
                    Code::Internal,
                    format!("filesystem watcher failure: {error}"),
                );
                record_watch_failure(&callback_failure, error);
                let _ = callback_sender.try_send(Err(RpcError::new(
                    Code::Internal,
                    "filesystem watcher failure",
                )));
                return;
            }
        };
        for path in event.paths {
            let Some(kind) = watch_event_kind(event.kind) else {
                continue;
            };
            let name = path
                .strip_prefix(&watch_root)
                .unwrap_or(&path)
                .display()
                .to_string();
            let entry = if include_entry
                && matches!(
                    kind,
                    proto::EventType::Create | proto::EventType::Write | proto::EventType::Chmod
                ) {
                entry_info_sync(&path).ok().map(proto_entry)
            } else {
                None
            };
            let payload = proto::WatchDirResponse {
                event: Some(proto::watch_dir_response::Event::Filesystem(
                    proto::FilesystemEvent {
                        name,
                        r#type: kind as i32,
                        entry,
                    },
                )),
            };
            let message = match wire::encode_json(&payload)
                .ok()
                .and_then(|payload| encode_frame(0, &payload).ok())
            {
                Some(message) => message,
                None => continue,
            };
            if callback_sender.try_send(Ok(message)).is_err() {
                record_watch_failure(
                    &callback_failure,
                    RpcError::new(Code::ResourceExhausted, "WatchDir subscriber is too slow"),
                );
                return;
            }
        }
    })
    .map_err(|error| {
        RpcError::new(
            Code::Internal,
            format!("create filesystem watcher: {error}"),
        )
    })?;
    watcher
        .watch(
            &path,
            if request.recursive {
                RecursiveMode::Recursive
            } else {
                RecursiveMode::NonRecursive
            },
        )
        .map_err(|error| {
            RpcError::new(Code::Internal, format!("watch {}: {error}", path.display()))
        })?;

    let start = watch_frame(proto::WatchDirResponse {
        event: Some(proto::watch_dir_response::Event::Start(
            proto::watch_dir_response::StartEvent {},
        )),
    });
    let output = WatchStream {
        start: Some(start),
        receiver,
        failure,
        watcher: Some(watcher),
        shutdown_wait: Box::pin(state.shutdown.clone().cancelled_owned()),
        keepalive,
        keepalive_sleep: Box::pin(tokio::time::sleep(keepalive)),
        ended: false,
    };

    Ok((
        [("content-type", "application/connect+json")],
        Body::from_stream(output),
    )
        .into_response())
}

/// 保存 WatchDir 响应流所需的 watcher、队列和生命周期状态。
struct WatchStream {
    /// 首条必须发送的 start 帧。
    start: Option<Vec<u8>>,
    /// 接收 notify 回调转换后的 Connect 帧。
    receiver: mpsc::Receiver<Result<Vec<u8>, RpcError>>,
    /// 保存最先发生的 watcher 失败原因。
    failure: Arc<StdMutex<Option<RpcError>>>,
    /// 持有 watcher 以维持监听生命周期。
    watcher: Option<notify::RecommendedWatcher>,
    /// 等待全局应用关闭信号。
    shutdown_wait: Pin<Box<dyn Future<Output = ()> + Send>>,
    /// 两次保活帧之间的间隔。
    keepalive: std::time::Duration,
    /// 驱动下一次保活帧的计时器。
    keepalive_sleep: Pin<Box<tokio::time::Sleep>>,
    /// 标记结束帧已经发送，避免重复关闭。
    ended: bool,
}

/// 将 watcher 队列和生命周期事件暴露为 Axum 可发送的字节流。
impl Stream for WatchStream {
    type Item = Result<StreamBytes, std::convert::Infallible>;

    /// 按 start、关闭、失败、保活和队列事件的优先级产出下一帧。
    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        if let Some(start) = self.start.take() {
            return std::task::Poll::Ready(Some(Ok(StreamBytes::from(start))));
        }
        if self.ended {
            return std::task::Poll::Ready(None);
        }
        if self.shutdown_wait.as_mut().poll(context).is_ready() {
            self.ended = true;
            self.watcher.take();
            return std::task::Poll::Ready(Some(Ok(StreamBytes::from(end_stream(None)))));
        }
        if let Some(error) = self
            .failure
            .lock()
            .ok()
            .and_then(|mut failure| failure.take())
        {
            self.ended = true;
            self.watcher.take();
            return std::task::Poll::Ready(Some(Ok(StreamBytes::from(end_stream(Some(error))))));
        }
        if self.keepalive_sleep.as_mut().poll(context).is_ready() {
            let keepalive = self.keepalive;
            self.keepalive_sleep
                .as_mut()
                .reset(tokio::time::Instant::now() + keepalive);
            let keepalive = watch_frame(proto::WatchDirResponse {
                event: Some(proto::watch_dir_response::Event::Keepalive(
                    proto::watch_dir_response::KeepAlive {},
                )),
            });
            return std::task::Poll::Ready(Some(Ok(StreamBytes::from(keepalive))));
        }
        match std::pin::Pin::new(&mut self.receiver).poll_recv(context) {
            std::task::Poll::Ready(Some(Ok(message))) => {
                let keepalive = self.keepalive;
                self.keepalive_sleep
                    .as_mut()
                    .reset(tokio::time::Instant::now() + keepalive);
                std::task::Poll::Ready(Some(Ok(StreamBytes::from(message))))
            }
            std::task::Poll::Ready(Some(Err(error))) => {
                self.ended = true;
                self.watcher.take();
                std::task::Poll::Ready(Some(Ok(StreamBytes::from(end_stream(Some(error))))))
            }
            std::task::Poll::Ready(None) => {
                self.ended = true;
                self.watcher.take();
                std::task::Poll::Ready(Some(Ok(StreamBytes::from(end_stream(None)))))
            }
            std::task::Poll::Pending => std::task::Poll::Pending,
        }
    }
}

// 只记录第一个 watcher 失败，保留最接近根因的错误信息。

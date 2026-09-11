// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use super::{framing, grpc, json, limits};
use axum::{
    body::Body,
    middleware::{self, Next},
    response::{IntoResponse, Response},
    Router,
};
use connectrpc::{Dispatcher, Router as ConnectRouter};
use http::Request;

pub(super) fn register(mut router: Router, connect: ConnectRouter) -> Router {
    let methods: Vec<_> = connect
        .methods()
        .map(|path| {
            (
                format!("/{path}"),
                connect.lookup(path).expect("registered method").kind,
            )
        })
        .collect();
    let connect = connect
        .into_axum_service()
        .with_limits(limits::connect_limits())
        .with_compression_policy(connectrpc::CompressionPolicy::default().with_min_size(0))
        .with_interceptor(limits::BudgetErrorInterceptor)
        .with_interceptor(json::ProtoJson);

    for (path, kind) in methods {
        router = router.route(
            &path,
            axum::routing::post_service(connect.clone()).layer(middleware::from_fn(
                move |request, next| rpc_transport(kind, request, next),
            )),
        );
    }
    router
}

const UNARY_ACCEPT_POST: &str = "application/grpc, application/grpc+json, application/grpc+json; charset=utf-8, application/grpc+proto, application/grpc-web, application/grpc-web+json, application/grpc-web+json; charset=utf-8, application/grpc-web+proto, application/json, application/json; charset=utf-8, application/proto";

const STREAM_ACCEPT_POST: &str = "application/connect+json, application/connect+json; charset=utf-8, application/connect+proto, application/grpc, application/grpc+json, application/grpc+json; charset=utf-8, application/grpc+proto, application/grpc-web, application/grpc-web+json, application/grpc-web+json; charset=utf-8, application/grpc-web+proto";

fn rpc_media_supported(kind: connectrpc::router::MethodKind, value: &str) -> bool {
    let mut parts = value.split(';');
    let media = parts.next().unwrap_or("").trim();
    if let Some(parameter) = parts.next() {
        if !media.ends_with("json") || parts.next().is_some() {
            return false;
        }
        let Some((key, value)) = parameter.trim().split_once('=') else {
            return false;
        };
        if key.trim() != "charset" || !matches!(value.trim(), "utf-8" | "\"utf-8\"") {
            return false;
        }
    }
    matches!(
        media,
        "application/grpc"
            | "application/grpc+proto"
            | "application/grpc+json"
            | "application/grpc-web"
            | "application/grpc-web+proto"
            | "application/grpc-web+json"
    ) || if matches!(kind, connectrpc::router::MethodKind::Unary) {
        matches!(media, "application/json" | "application/proto")
    } else {
        matches!(
            media,
            "application/connect+json" | "application/connect+proto"
        )
    }
}

async fn rpc_transport(
    kind: connectrpc::router::MethodKind,
    request: Request<Body>,
    next: Next,
) -> Response {
    if request.method() != http::Method::POST {
        return next.run(request).await;
    }
    let content_type = request
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    if !rpc_media_supported(kind, content_type) {
        let accept = if matches!(kind, connectrpc::router::MethodKind::Unary) {
            UNARY_ACCEPT_POST
        } else {
            STREAM_ACCEPT_POST
        };
        return (
            http::StatusCode::UNSUPPORTED_MEDIA_TYPE,
            [("accept-post", accept)],
        )
            .into_response();
    }
    let connect_stream = content_type.starts_with("application/connect+");
    let grpc = content_type.starts_with("application/grpc");
    let grpc_web = content_type.starts_with("application/grpc-web");
    let legacy = request.uri().path().starts_with("/filesystem.Filesystem/")
        && request
            .headers()
            .get("user-agent")
            .is_some_and(|value| value == "connect-python");
    let json_charset =
        content_type.starts_with("application/json;") && content_type.contains("charset");
    let connect_unary = matches!(kind, connectrpc::router::MethodKind::Unary)
        && matches!(
            content_type.split(';').next().unwrap_or("").trim(),
            "application/json" | "application/proto"
        );
    let protocol_version = request
        .headers()
        .get("connect-protocol-version")
        .and_then(|value| value.to_str().ok())
        .unwrap_or("")
        .to_owned();
    let request_compression = request
        .headers()
        .get("content-encoding")
        .and_then(|value| value.to_str().ok())
        .unwrap_or("")
        .to_owned();
    let stream_compression = connectrpc::CompressionRegistry::default().negotiate_encoding(
        request
            .headers()
            .get("connect-accept-encoding")
            .and_then(|value| value.to_str().ok()),
        request
            .headers()
            .get("connect-content-encoding")
            .and_then(|value| value.to_str().ok()),
    ) == Some("gzip");
    let mut response = match framing::request(kind, request).await {
        Ok(request) => next.run(request).await,
        Err(response) => response,
    };
    if connect_unary
        && matches!(
            response.status(),
            http::StatusCode::BAD_REQUEST | http::StatusCode::NOT_IMPLEMENTED
        )
    {
        let (mut parts, body) = response.into_parts();
        let bytes = match axum::body::to_bytes(body, usize::MAX).await {
            Ok(bytes) => bytes,
            Err(_) => return http::StatusCode::INTERNAL_SERVER_ERROR.into_response(),
        };
        let mut replacement = None;
        if let Ok(mut error) = serde_json::from_slice::<serde_json::Value>(&bytes) {
            if let Some(message) = error.get("message").and_then(|value| value.as_str()) {
                let mapped = if message == "unsupported protocol version" {
                    // Go negotiates compression before validating the version.
                    if !matches!(request_compression.as_str(), "" | "identity" | "gzip") {
                        parts.status = http::StatusCode::NOT_IMPLEMENTED;
                        error["code"] = "unimplemented".into();
                        Some(format!(
                            "unknown compression {request_compression:?}: supported encodings are gzip"
                        ))
                    } else {
                        Some(format!(
                            "Connect-Protocol-Version must be \"1\": got {protocol_version:?}"
                        ))
                    }
                } else if matches!(
                    message,
                    "gzip data too short for header" | "gzip header truncated"
                ) {
                    Some("get decompressor: unexpected EOF".into())
                } else {
                    message
                        .strip_prefix("unsupported compression encoding: ")
                        .map(|encoding| {
                            format!(
                                "unknown compression {encoding:?}: supported encodings are gzip"
                            )
                        })
                };
                if let Some(message) = mapped {
                    error["message"] = message.into();
                    replacement = serde_json::to_vec(&error).ok();
                }
            }
        }
        if let Some(bytes) = replacement {
            parts.headers.remove(http::header::CONTENT_LENGTH);
            response = Response::from_parts(parts, Body::from(bytes));
        } else {
            response = Response::from_parts(parts, Body::from(bytes));
        }
    }
    if grpc {
        response = grpc::response(response, grpc_web).await;
    }
    if connect_stream && stream_compression {
        response = framing::compressed_response(response);
    }
    if connect_stream {
        response.headers_mut().insert(
            "connect-accept-encoding",
            http::HeaderValue::from_static("gzip"),
        );
    } else if grpc {
        response.headers_mut().insert(
            "grpc-accept-encoding",
            http::HeaderValue::from_static("gzip"),
        );
    }
    if legacy && !matches!(kind, connectrpc::router::MethodKind::Unary) {
        response
            .headers_mut()
            .insert("x-e2b-legacy-sdk", http::HeaderValue::from_static("true"));
    }
    if connect_unary && response.status() != http::StatusCode::UNSUPPORTED_MEDIA_TYPE {
        response
            .headers_mut()
            .insert("accept-encoding", http::HeaderValue::from_static("gzip"));
        if json_charset && response.status().is_success() {
            response.headers_mut().insert(
                "content-type",
                http::HeaderValue::from_static("application/json; charset=utf-8"),
            );
        }
    }
    if matches!(kind, connectrpc::router::MethodKind::Unary)
        && response.status() == axum::http::StatusCode::UNSUPPORTED_MEDIA_TYPE
    {
        response.headers_mut().insert(
            "accept-post",
            axum::http::HeaderValue::from_static(UNARY_ACCEPT_POST),
        );
    }
    response
}

use connectrpc::{ConnectError, RequestContext, ServiceRequest, ServiceResult};

use crate::proto::process::{
    CloseStdinRequest, CloseStdinResponse, ConnectRequest, ConnectResponse, ListRequest,
    ListResponse, Process as ProcessRpc, SendInputRequest, SendInputResponse, SendSignalRequest,
    SendSignalResponse, StartRequest, StartResponse, StreamInputRequest, StreamInputResponse,
    UpdateRequest, UpdateResponse,
};

fn process_io_error(error: crate::error::DomainError) -> ConnectError {
    let code = if matches!(error, crate::error::DomainError::FailedPrecondition(_)) {
        connectrpc::ErrorCode::Internal
    } else {
        error.connect_code()
    };
    ConnectError::new(code, error.public_message())
}

pub struct ProcessConnectService(pub std::sync::Arc<super::rest::AppState>);

#[allow(refining_impl_trait)]
impl ProcessRpc for ProcessConnectService {
    async fn list(
        &self,
        _ctx: RequestContext,
        _req: ServiceRequest<'_, ListRequest>,
    ) -> ServiceResult<ListResponse> {
        let response = self
            .0
            .processes
            .list()
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        connectrpc::Response::ok(response)
    }

    async fn connect(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, ConnectRequest>,
    ) -> ServiceResult<connectrpc::ServiceStream<ConnectResponse>> {
        let deadline = super::timeout::subscription_deadline(&ctx)?;
        let interval = crate::process::output::keepalive(ctx.headers())
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        let subscription = self
            .0
            .processes
            .connect(req.to_owned_message())
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        use futures::StreamExt;
        connectrpc::Response::ok(super::timeout::with_subscription_deadline(
            Box::pin(subscription.stream(interval).map(|event| {
                event.map(|event| ConnectResponse {
                    event: event.into(),
                    ..Default::default()
                })
            })) as connectrpc::ServiceStream<ConnectResponse>,
            deadline,
        ))
    }

    async fn start(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, StartRequest>,
    ) -> ServiceResult<connectrpc::ServiceStream<StartResponse>> {
        let deadline = super::timeout::subscription_deadline(&ctx)?;
        let interval = crate::process::output::keepalive(ctx.headers())
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        let mut headers = ctx.headers().clone();
        if let Some(timeout) = ctx.extensions().get::<super::timeout::ProcessTimeout>() {
            headers.insert("connect-timeout-ms", timeout.0.clone());
        }
        let snapshot = self.0.runtime.snapshot().await;
        let subscription = self
            .0
            .processes
            .start(
                req.to_owned_message(),
                &headers,
                snapshot,
                &self.0.users,
                self.0.lifecycle.clone(),
            )
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        use futures::StreamExt;
        connectrpc::Response::ok(super::timeout::with_subscription_deadline(
            Box::pin(subscription.stream(interval).map(|event| {
                event.map(|event| StartResponse {
                    event: event.into(),
                    ..Default::default()
                })
            })) as connectrpc::ServiceStream<StartResponse>,
            deadline,
        ))
    }

    async fn update(
        &self,
        _ctx: RequestContext,
        req: ServiceRequest<'_, UpdateRequest>,
    ) -> ServiceResult<UpdateResponse> {
        self.0
            .processes
            .update(req.to_owned_message())
            .map_err(process_io_error)?;
        connectrpc::Response::ok(Default::default())
    }

    async fn stream_input(
        &self,
        _ctx: RequestContext,
        mut req: connectrpc::InboundStream<StreamInputRequest>,
    ) -> ServiceResult<StreamInputResponse> {
        use crate::proto::process::stream_input_request::Event;
        use futures::StreamExt;
        let mut target = None;
        while let Some(message) = req.next().await {
            let message = message.map_err(|error| {
                ConnectError::new(
                    connectrpc::ErrorCode::Unknown,
                    format!("error streaming input: {error}"),
                )
            })?;
            match message.to_owned_message().event {
                Some(Event::Start(start)) => {
                    target = Some(
                        self.0
                            .processes
                            .input_target(&start.process)
                            .map_err(process_io_error)?,
                    );
                }
                Some(Event::Data(data)) => {
                    let target = target.as_ref().ok_or_else(|| {
                        ConnectError::new(
                            connectrpc::ErrorCode::Internal,
                            "input received before start",
                        )
                    })?;
                    target
                        .send(data.input.into_option())
                        .await
                        .map_err(process_io_error)?;
                }
                Some(Event::Keepalive(_)) => {}
                None => {
                    return Err(ConnectError::new(
                        connectrpc::ErrorCode::Unimplemented,
                        "invalid event type <nil>",
                    ))
                }
            }
        }
        connectrpc::Response::ok(Default::default())
    }

    async fn send_input(
        &self,
        _ctx: RequestContext,
        req: ServiceRequest<'_, SendInputRequest>,
    ) -> ServiceResult<SendInputResponse> {
        self.0
            .processes
            .send_input(req.to_owned_message())
            .await
            .map_err(process_io_error)?;
        connectrpc::Response::ok(Default::default())
    }

    async fn send_signal(
        &self,
        _ctx: RequestContext,
        req: ServiceRequest<'_, SendSignalRequest>,
    ) -> ServiceResult<SendSignalResponse> {
        self.0
            .processes
            .send_signal(req.to_owned_message())
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        connectrpc::Response::ok(Default::default())
    }

    async fn close_stdin(
        &self,
        _ctx: RequestContext,
        req: ServiceRequest<'_, CloseStdinRequest>,
    ) -> ServiceResult<CloseStdinResponse> {
        self.0
            .processes
            .close_stdin(req.to_owned_message())
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        connectrpc::Response::ok(Default::default())
    }
}

use crate::proto::filesystem::{Filesystem as FilesystemRpc, WatchDirRequest, WatchDirResponse};

fn legacy_entry(
    ctx: &RequestContext,
    entry: crate::proto::filesystem::EntryInfo,
) -> crate::proto::filesystem::EntryInfo {
    if ctx
        .headers()
        .get("user-agent")
        .is_some_and(|value| value == "connect-python")
    {
        crate::proto::filesystem::EntryInfo {
            name: entry.name,
            path: entry.path,
            r#type: entry.r#type,
            ..Default::default()
        }
    } else {
        entry
    }
}

fn legacy_ok<T>(ctx: &RequestContext, value: T) -> ServiceResult<T> {
    let mut response = connectrpc::Response::new(value);
    if ctx
        .headers()
        .get("user-agent")
        .is_some_and(|value| value == "connect-python")
    {
        response = response.with_header("x-e2b-legacy-sdk", "true");
    }
    Ok(response)
}

pub struct FilesystemConnectService(pub std::sync::Arc<super::rest::AppState>);

#[allow(refining_impl_trait)]
impl FilesystemRpc for FilesystemConnectService {
    async fn stat(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, crate::proto::filesystem::StatRequest>,
    ) -> ServiceResult<crate::proto::filesystem::StatResponse> {
        let snapshot = self.0.runtime.snapshot().await;
        let entry = self
            .0
            .filesystem
            .stat(
                req.to_owned_message().path,
                ctx.headers(),
                snapshot,
                self.0.users.clone(),
            )
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        legacy_ok(
            &ctx,
            crate::proto::filesystem::StatResponse {
                entry: legacy_entry(&ctx, entry).into(),
                ..Default::default()
            },
        )
    }

    async fn make_dir(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, crate::proto::filesystem::MakeDirRequest>,
    ) -> ServiceResult<crate::proto::filesystem::MakeDirResponse> {
        let snapshot = self.0.runtime.snapshot().await;
        let entry = self
            .0
            .filesystem
            .make_dir(
                req.to_owned_message().path,
                ctx.headers(),
                snapshot,
                self.0.users.clone(),
            )
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        legacy_ok(
            &ctx,
            crate::proto::filesystem::MakeDirResponse {
                entry: legacy_entry(&ctx, entry).into(),
                ..Default::default()
            },
        )
    }

    async fn r#move(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, crate::proto::filesystem::MoveRequest>,
    ) -> ServiceResult<crate::proto::filesystem::MoveResponse> {
        let snapshot = self.0.runtime.snapshot().await;
        let request = req.to_owned_message();
        let entry = self
            .0
            .filesystem
            .move_entry(
                request.source,
                request.destination,
                ctx.headers(),
                snapshot,
                self.0.users.clone(),
            )
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        legacy_ok(
            &ctx,
            crate::proto::filesystem::MoveResponse {
                entry: legacy_entry(&ctx, entry).into(),
                ..Default::default()
            },
        )
    }

    async fn list_dir(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, crate::proto::filesystem::ListDirRequest>,
    ) -> ServiceResult<crate::proto::filesystem::ListDirResponse> {
        let snapshot = self.0.runtime.snapshot().await;
        let request = req.to_owned_message();
        let entries = self
            .0
            .filesystem
            .list_dir(
                request.path,
                request.depth,
                ctx.headers(),
                snapshot,
                self.0.users.clone(),
            )
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        legacy_ok(
            &ctx,
            crate::proto::filesystem::ListDirResponse {
                entries: entries
                    .into_iter()
                    .map(|entry| legacy_entry(&ctx, entry))
                    .collect(),
                ..Default::default()
            },
        )
    }

    async fn remove(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, crate::proto::filesystem::RemoveRequest>,
    ) -> ServiceResult<crate::proto::filesystem::RemoveResponse> {
        let snapshot = self.0.runtime.snapshot().await;
        self.0
            .filesystem
            .remove(
                req.to_owned_message().path,
                ctx.headers(),
                snapshot,
                self.0.users.clone(),
            )
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        legacy_ok(&ctx, Default::default())
    }

    async fn watch_dir(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, WatchDirRequest>,
    ) -> ServiceResult<connectrpc::ServiceStream<WatchDirResponse>> {
        let interval = crate::process::output::keepalive(ctx.headers())
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        let snapshot = self.0.runtime.snapshot().await;
        let subscription = self
            .0
            .filesystem
            .watch_dir(
                req.to_owned_message(),
                ctx.headers(),
                snapshot,
                self.0.users.clone(),
                self.0.lifecycle.clone(),
            )
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        connectrpc::Response::ok(subscription.stream(interval))
    }

    async fn create_watcher(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, crate::proto::filesystem::CreateWatcherRequest>,
    ) -> ServiceResult<crate::proto::filesystem::CreateWatcherResponse> {
        let snapshot = self.0.runtime.snapshot().await;
        let watcher_id = self
            .0
            .filesystem
            .create_watcher(
                req.to_owned_message(),
                ctx.headers(),
                snapshot,
                self.0.users.clone(),
                self.0.lifecycle.clone(),
            )
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        #[cfg(test)]
        let _attempt = crate::filesystem::snapshot_test::attempt_at(
            "WatcherCreateResponse",
            ctx.headers()
                .get("x-snapshot-test-tag")
                .and_then(|value| value.to_str().ok()),
        );
        #[cfg(test)]
        crate::filesystem::snapshot_test::barrier(
            "WatcherPublished",
            ctx.headers()
                .get("x-snapshot-test-tag")
                .and_then(|value| value.to_str().ok()),
        )
        .await
        .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        legacy_ok(
            &ctx,
            crate::proto::filesystem::CreateWatcherResponse {
                watcher_id,
                ..Default::default()
            },
        )
    }

    async fn get_watcher_events(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, crate::proto::filesystem::GetWatcherEventsRequest>,
    ) -> ServiceResult<crate::proto::filesystem::GetWatcherEventsResponse> {
        let response = self
            .0
            .filesystem
            .get_watcher_events(&req.to_owned_message().watcher_id)
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        #[cfg(test)]
        let _attempt = crate::filesystem::snapshot_test::attempt_at(
            "WatcherGet",
            ctx.headers()
                .get("x-snapshot-test-tag")
                .and_then(|value| value.to_str().ok()),
        );
        #[cfg(test)]
        crate::filesystem::snapshot_test::barrier(
            "WatcherDrained",
            ctx.headers()
                .get("x-snapshot-test-tag")
                .and_then(|value| value.to_str().ok()),
        )
        .await
        .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        legacy_ok(&ctx, response)
    }

    async fn remove_watcher(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, crate::proto::filesystem::RemoveWatcherRequest>,
    ) -> ServiceResult<crate::proto::filesystem::RemoveWatcherResponse> {
        self.0
            .filesystem
            .remove_watcher(&req.to_owned_message().watcher_id)
            .await
            .map_err(|error| ConnectError::new(error.connect_code(), error.public_message()))?;
        legacy_ok(&ctx, Default::default())
    }
}

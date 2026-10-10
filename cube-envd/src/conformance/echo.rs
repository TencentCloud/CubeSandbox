// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use connectrpc::{ConnectError, RequestContext, Response, ServiceRequest, ServiceResult};
use futures::StreamExt as _;

use crate::error::DomainError;
use crate::proto::conformance::echo::{
    echo_request, echo_stream_item, Echo, EchoClientItem, EchoEnum, EchoRequest, EchoResponse,
    EchoStreamItem,
};

use crate::transport::timeout::parse_connect_timeout_ms;

pub struct EchoGateService;

#[allow(refining_impl_trait)]
impl Echo for EchoGateService {
    async fn unary(
        &self,
        ctx: RequestContext,
        req: ServiceRequest<'_, EchoRequest>,
    ) -> ServiceResult<EchoResponse> {
        if matches!(
            parse_connect_timeout_ms(
                ctx.headers()
                    .get("connect-timeout-ms")
                    .and_then(|value| value.to_str().ok()),
            ),
            crate::transport::timeout::ConnectTimeoutPolicy::Invalid
        ) {
            return Err(ConnectError::new(
                DomainError::InvalidArgument("invalid Connect-Timeout-Ms".into()).connect_code(),
                "invalid Connect-Timeout-Ms",
            ));
        }

        if req.message == "error" {
            return Err(ConnectError::new(
                DomainError::InvalidArgument("forced error".into()).connect_code(),
                "forced error",
            ));
        }

        let req = req.to_owned_message();
        let selected_name = match &req.selector {
            Some(echo_request::Selector::Name(name)) => name.clone(),
            Some(echo_request::Selector::Id(id)) => id.to_string(),
            None => String::new(),
        };

        Response::ok(EchoResponse {
            message: req.message,
            kind: req.kind,
            big_number: req.big_number,
            payload: req.payload,
            selected_name,
            ..Default::default()
        })
    }

    async fn server_stream(
        &self,
        _ctx: RequestContext,
        req: ServiceRequest<'_, EchoRequest>,
    ) -> ServiceResult<connectrpc::ServiceStream<EchoStreamItem>> {
        let payload = req.payload.to_vec();
        let items = vec![
            EchoStreamItem {
                event: Some(echo_stream_item::Event::Start(Box::default())),
                ..Default::default()
            },
            EchoStreamItem {
                event: Some(echo_stream_item::Event::Data(Box::new(
                    echo_stream_item::StreamData {
                        payload,
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            },
            EchoStreamItem {
                event: Some(echo_stream_item::Event::End(Box::new(
                    echo_stream_item::StreamEnd {
                        ok: true,
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            },
        ];
        Response::ok(stream_from_vec(items))
    }

    async fn client_stream(
        &self,
        _ctx: RequestContext,
        mut req: connectrpc::InboundStream<EchoClientItem>,
    ) -> ServiceResult<EchoResponse> {
        let mut total = 0usize;
        while let Some(message) = req.next().await {
            total += message?.chunk().len();
        }
        Response::ok(EchoResponse {
            message: format!("bytes={total}"),
            kind: EchoEnum::Alpha.into(),
            ..Default::default()
        })
    }
}

fn stream_from_vec<T: Send + 'static>(items: Vec<T>) -> connectrpc::ServiceStream<T> {
    Box::pin(futures::stream::iter(items.into_iter().map(Ok)))
}

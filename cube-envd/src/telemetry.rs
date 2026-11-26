// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use crate::cli::LogFormat;
use axum::extract::{MatchedPath, Request, State};
use axum::middleware::Next;
use axum::response::Response;
use opentelemetry::propagation::TextMapPropagator;
use opentelemetry::trace::{FutureExt, SpanKind, Status, TraceContextExt, Tracer, TracerProvider};
use opentelemetry::KeyValue;
use opentelemetry_http::HeaderExtractor;
use opentelemetry_otlp::{Protocol, SpanExporter, WithExportConfig};
use opentelemetry_sdk::propagation::TraceContextPropagator;
use opentelemetry_sdk::trace::{SdkTracer, SdkTracerProvider};
use opentelemetry_sdk::Resource;
use std::sync::OnceLock;
use tracing_subscriber::{fmt, prelude::*, EnvFilter};

pub type LogReceiver = tokio::sync::mpsc::UnboundedReceiver<Vec<u8>>;

#[derive(Clone)]
struct LogSender(tokio::sync::mpsc::UnboundedSender<Vec<u8>>);

struct LogWriter {
    sender: tokio::sync::mpsc::UnboundedSender<Vec<u8>>,
    buffer: Vec<u8>,
}

impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for LogSender {
    type Writer = LogWriter;

    fn make_writer(&'a self) -> Self::Writer {
        LogWriter {
            sender: self.0.clone(),
            buffer: Vec::new(),
        }
    }
}

impl std::io::Write for LogWriter {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        self.buffer.extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

impl Drop for LogWriter {
    fn drop(&mut self) {
        for line in self.buffer.split(|byte| *byte == b'\n') {
            if !line.is_empty() {
                let _ = self.sender.send(line.to_vec());
            }
        }
    }
}

pub fn init(format: LogFormat) -> LogReceiver {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    let (sender, receiver) = tokio::sync::mpsc::unbounded_channel();
    match format {
        LogFormat::Text => {
            tracing_subscriber::registry()
                .with(filter)
                .with(fmt::layer())
                .with(
                    fmt::layer()
                        .json()
                        .with_ansi(false)
                        .with_writer(LogSender(sender)),
                )
                .init();
        }
        LogFormat::Json => {
            tracing_subscriber::registry()
                .with(filter)
                .with(fmt::layer().json())
                .with(
                    fmt::layer()
                        .json()
                        .with_ansi(false)
                        .with_writer(LogSender(sender)),
                )
                .init();
        }
    }
    receiver
}

pub fn redact_secret(value: &str) -> String {
    if value.is_empty() {
        return String::new();
    }
    format!("[REDACTED len={}]", value.len())
}

const TRACER_SCOPE: &str = "github.com/tencentcloud/CubeSandbox/cube-envd";
static HTTP_TRACER: OnceLock<SdkTracer> = OnceLock::new();

pub struct TraceGuard(SdkTracerProvider);

impl Drop for TraceGuard {
    fn drop(&mut self) {
        if let Err(error) = self.0.shutdown() {
            tracing::warn!(%error, "could not shut down the trace exporter");
        }
    }
}

/// Enable REST tracing when a guest-reachable OTLP/HTTP traces URL is configured.
pub fn init_tracing() -> Option<TraceGuard> {
    let endpoint = std::env::var("CUBE_ENVD_OTEL_ENDPOINT").ok()?;
    let endpoint = endpoint.trim();
    if endpoint.is_empty() {
        return None;
    }
    if !endpoint.parse::<http::Uri>().is_ok_and(|uri| {
        matches!(uri.scheme_str(), Some("http" | "https")) && uri.authority().is_some()
    }) {
        tracing::warn!("invalid CUBE_ENVD_OTEL_ENDPOINT, tracing disabled");
        return None;
    }

    let exporter = match SpanExporter::builder()
        .with_http()
        .with_protocol(Protocol::HttpBinary)
        .with_endpoint(endpoint)
        .build()
    {
        Ok(exporter) => exporter,
        Err(error) => {
            tracing::warn!(%error, "could not initialize the trace exporter");
            return None;
        }
    };
    let service = std::env::var("OTEL_SERVICE_NAME")
        .ok()
        .filter(|value| !value.trim().is_empty())
        .unwrap_or_else(|| "envd".to_string());
    let provider = SdkTracerProvider::builder()
        .with_batch_exporter(exporter)
        .with_resource(Resource::builder().with_service_name(service).build())
        .build();
    HTTP_TRACER.set(provider.tracer(TRACER_SCOPE)).ok()?;
    Some(TraceGuard(provider))
}

pub(crate) fn http_tracer() -> Option<SdkTracer> {
    HTTP_TRACER.get().cloned()
}

pub(crate) async fn trace_http_request(
    State(tracer): State<SdkTracer>,
    request: Request,
    next: Next,
) -> Response {
    let parent = TraceContextPropagator::new().extract(&HeaderExtractor(request.headers()));
    if !parent.span().span_context().is_sampled() {
        return next.run(request).await;
    }
    let Some(route) = request.extensions().get::<MatchedPath>() else {
        return next.run(request).await;
    };

    let method = request.method().to_string();
    let route = route.as_str().to_owned();
    let span = tracer
        .span_builder(format!("{method} {route}"))
        .with_kind(SpanKind::Server)
        .with_attributes([
            KeyValue::new("http.request.method", method),
            KeyValue::new("http.route", route),
        ])
        .start_with_context(&tracer, &parent);
    let context = parent.with_span(span);
    let response = next.run(request).with_context(context.clone()).await;
    let span = context.span();
    let status = response.status();
    span.set_attribute(KeyValue::new(
        "http.response.status_code",
        i64::from(status.as_u16()),
    ));
    if status.is_server_error() {
        span.set_status(Status::error(status.to_string()));
    }
    span.end();
    response
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn rest_trace_uses_the_inbound_parent_and_matched_route() {
        use axum::{body::Body, http::StatusCode, middleware, routing::get, Router};
        use opentelemetry_sdk::trace::InMemorySpanExporter;
        use tower::ServiceExt;

        let exporter = InMemorySpanExporter::default();
        let provider = SdkTracerProvider::builder()
            .with_simple_exporter(exporter.clone())
            .build();
        let router = Router::new()
            .route(
                "/files/{key}",
                get(|| async { StatusCode::INTERNAL_SERVER_ERROR }),
            )
            .route_layer(middleware::from_fn_with_state(
                provider.tracer(TRACER_SCOPE),
                trace_http_request,
            ));

        for header in [
            None,
            Some("invalid"),
            Some("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"),
            Some("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"),
        ] {
            let mut request = Request::builder().uri("/files/variable?query=ignored");
            if let Some(header) = header {
                request = request.header("traceparent", header);
            }
            let response = router
                .clone()
                .oneshot(request.body(Body::empty()).unwrap())
                .await
                .unwrap();
            assert_eq!(response.status(), StatusCode::INTERNAL_SERVER_ERROR);
        }

        let spans = exporter.get_finished_spans().unwrap();
        assert_eq!(spans.len(), 1);
        let span = &spans[0];
        assert_eq!(span.name, "GET /files/{key}");
        assert_eq!(span.span_kind, SpanKind::Server);
        assert_eq!(
            span.span_context.trace_id().to_string(),
            "4bf92f3577b34da6a3ce929d0e0e4736"
        );
        assert_eq!(span.parent_span_id.to_string(), "00f067aa0ba902b7");
        assert!(matches!(span.status, Status::Error { .. }));
        assert!(span
            .attributes
            .contains(&KeyValue::new("http.response.status_code", 500_i64)));
    }

    #[test]
    fn redact_does_not_leak_content() {
        let secret = "super-secret-token-value";
        let redacted = redact_secret(secret);
        assert!(!redacted.contains("super-secret"));
        assert!(redacted.contains("len="));
    }
}

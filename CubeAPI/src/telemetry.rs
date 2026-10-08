// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Tracing for sandbox creation.

use axum::{extract::Request, http::HeaderMap, middleware::Next, response::Response};
use opentelemetry::{
    global,
    trace::{FutureExt, SpanKind, Status, TraceContextExt, Tracer},
    Context, KeyValue,
};
use opentelemetry_http::{HeaderExtractor, HeaderInjector};
use opentelemetry_otlp::{SpanExporter, WithExportConfig};
use opentelemetry_sdk::{propagation::TraceContextPropagator, trace::SdkTracerProvider, Resource};

const ENV_ENDPOINT: &str = "OTEL_EXPORTER_OTLP_ENDPOINT";
const ENV_SERVICE_NAME: &str = "OTEL_SERVICE_NAME";

const DEFAULT_SERVICE_NAME: &str = "cube-api";

const SCOPE: &str = "github.com/tencentcloud/CubeSandbox/CubeAPI";

const CREATE_METHOD: &str = "POST";
const CREATE_PATH: &str = "/sandboxes";
const CREATE_SPAN: &str = "POST /sandboxes";
const TEMPLATE_PATH: &str = "/templates";
const TEMPLATE_SPAN: &str = "POST /templates";

const TRACED_ROUTES: [(&str, &str); 2] =
    [(CREATE_PATH, CREATE_SPAN), (TEMPLATE_PATH, TEMPLATE_SPAN)];

const REQUEST_ID_HEADER: &str = "x-request-id";

pub struct Guard {
    provider: Option<SdkTracerProvider>,
}

impl Guard {
    pub fn shutdown(self) {
        if let Some(provider) = self.provider {
            let _ = provider.shutdown();
        }
    }
}

pub fn init() -> Guard {
    let endpoint = std::env::var(ENV_ENDPOINT).unwrap_or_default();
    let endpoint = endpoint.trim().to_string();
    if endpoint.is_empty() {
        return Guard { provider: None };
    }

    let service = std::env::var(ENV_SERVICE_NAME)
        .ok()
        .filter(|v| !v.trim().is_empty())
        .unwrap_or_else(|| DEFAULT_SERVICE_NAME.to_string());

    match build_provider(&endpoint, &service) {
        Ok(provider) => {
            global::set_text_map_propagator(TraceContextPropagator::new());
            global::set_tracer_provider(provider.clone());
            Guard {
                provider: Some(provider),
            }
        }
        Err(err) => {
            tracing::warn!("telemetry setup failed, tracing disabled: {err}");
            Guard { provider: None }
        }
    }
}

fn build_provider(
    endpoint: &str,
    service: &str,
) -> Result<SdkTracerProvider, Box<dyn std::error::Error + Send + Sync>> {
    let exporter = SpanExporter::builder()
        .with_tonic()
        .with_endpoint(endpoint)
        .build()?;

    Ok(SdkTracerProvider::builder()
        .with_batch_exporter(exporter)
        .with_resource(
            Resource::builder()
                .with_service_name(service.to_string())
                .build(),
        )
        .build())
}

pub async fn layer(req: Request, next: Next) -> Response {
    let Some((_, span_name)) = TRACED_ROUTES
        .iter()
        .find(|(path, _)| *path == req.uri().path())
    else {
        return next.run(req).await;
    };
    if req.method().as_str() != CREATE_METHOD {
        return next.run(req).await;
    }

    let parent = global::get_text_map_propagator(|p| p.extract(&HeaderExtractor(req.headers())));
    let tracer = global::tracer(SCOPE);

    let mut attributes = vec![
        KeyValue::new("http.request.method", req.method().to_string()),
        KeyValue::new("url.path", req.uri().path().to_string()),
    ];
    if let Some(request_id) = req
        .headers()
        .get(REQUEST_ID_HEADER)
        .and_then(|value| value.to_str().ok())
    {
        attributes.push(KeyValue::new(
            "cube.http_request_id",
            request_id.to_string(),
        ));
    }
    let span = tracer
        .span_builder(*span_name)
        .with_kind(SpanKind::Server)
        .with_attributes(attributes)
        .start_with_context(&tracer, &parent);

    let cx = Context::current_with_span(span);
    let response = next.run(req).with_context(cx.clone()).await;

    let status = response.status();
    cx.span().set_attribute(KeyValue::new(
        "http.response.status_code",
        i64::from(status.as_u16()),
    ));
    if status.is_server_error() {
        cx.span().set_status(Status::error(status.to_string()));
    }
    cx.span().end();

    response
}

pub fn inject(headers: &mut HeaderMap) {
    global::get_text_map_propagator(|propagator| {
        propagator.inject_context(&Context::current(), &mut HeaderInjector(headers))
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        body::Body,
        http::{HeaderName, HeaderValue, Request as HttpRequest},
        routing::post,
        Router,
    };
    use opentelemetry_sdk::trace::InMemorySpanExporter;
    use tower::ServiceExt;

    const TRACEPARENT_HEADER: &str = "traceparent";
    const INBOUND_TRACE_ID: &str = "4bf92f3577b34da6a3ce929d0e0e4736";
    const INBOUND_PARENT_ID: &str = "00f067aa0ba902b7";
    const INBOUND: &str = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

    async fn ok() -> &'static str {
        "ok"
    }

    fn inbound() -> HeaderMap {
        let mut headers = HeaderMap::new();
        headers.insert(
            HeaderName::from_static(TRACEPARENT_HEADER),
            HeaderValue::from_static(INBOUND),
        );
        headers
    }

    #[tokio::test]
    async fn entry_routes_are_spanned_and_carry_the_trace() {
        let exporter = InMemorySpanExporter::default();
        global::set_text_map_propagator(TraceContextPropagator::new());
        global::set_tracer_provider(
            SdkTracerProvider::builder()
                .with_simple_exporter(exporter.clone())
                .build(),
        );

        let app = Router::new()
            .route("/sandboxes", post(ok).get(ok))
            .route("/templates", post(ok).get(ok))
            .route("/untraced", post(ok).get(ok))
            .layer(axum::middleware::from_fn(layer));

        for (uri, request_id) in [("/sandboxes", "req-1"), ("/templates", "req-2")] {
            let response = app
                .clone()
                .oneshot(
                    HttpRequest::builder()
                        .method("POST")
                        .uri(uri)
                        .header(TRACEPARENT_HEADER, INBOUND)
                        .header(REQUEST_ID_HEADER, request_id)
                        .body(Body::empty())
                        .expect("request"),
                )
                .await
                .expect("infallible");
            assert_eq!(response.status(), 200);
        }

        for (method, uri) in [
            ("GET", "/sandboxes"),
            ("GET", "/templates"),
            ("POST", "/untraced"),
        ] {
            let response = app
                .clone()
                .oneshot(
                    HttpRequest::builder()
                        .method(method)
                        .uri(uri)
                        .body(Body::empty())
                        .expect("request"),
                )
                .await
                .expect("infallible");
            assert_eq!(response.status(), 200);
        }

        let parent = global::get_text_map_propagator(|p| p.extract(&HeaderExtractor(&inbound())));
        let tracer = global::tracer(SCOPE);
        let child = tracer
            .span_builder("outbound")
            .start_with_context(&tracer, &parent);
        let guard = Context::current_with_span(child).attach();
        let mut injected = HeaderMap::new();
        inject(&mut injected);
        drop(guard);

        let cx = global::get_text_map_propagator(|p| p.extract(&HeaderExtractor(&injected)));
        assert!(
            injected.contains_key(TRACEPARENT_HEADER),
            "inject must write a header"
        );
        assert_eq!(
            cx.span().span_context().trace_id().to_string(),
            INBOUND_TRACE_ID,
            "injected traceparent must round-trip"
        );

        let spans = exporter.get_finished_spans().expect("finished spans");
        let created: Vec<_> = spans.iter().filter(|s| s.name == CREATE_SPAN).collect();
        assert_eq!(created.len(), 1, "the create route must be spanned once");
        let templates: Vec<_> = spans.iter().filter(|s| s.name == TEMPLATE_SPAN).collect();
        assert_eq!(
            templates.len(),
            1,
            "the template route must be spanned once"
        );

        for span in [created[0], templates[0]] {
            assert_eq!(span.span_kind, SpanKind::Server);
            assert_eq!(
                span.span_context.trace_id().to_string(),
                INBOUND_TRACE_ID,
                "the public entry span must continue the caller's trace"
            );
            assert_eq!(
                span.parent_span_id.to_string(),
                INBOUND_PARENT_ID,
                "the public entry span must hang under the inbound parent"
            );
        }
        assert!(
            spans.iter().all(|s| s.name == CREATE_SPAN
                || s.name == TEMPLATE_SPAN
                || &*s.name == "outbound"),
            "only the entry routes may open a span, got {:?}",
            spans.iter().map(|s| &*s.name).collect::<Vec<_>>()
        );
    }
}

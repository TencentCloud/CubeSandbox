// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;
use std::future::Future;

use opentelemetry::global::BoxedSpan;
use opentelemetry::propagation::Extractor;
use opentelemetry::{
    global,
    trace::{Span, SpanKind, Status, TraceContextExt, Tracer},
    Context, KeyValue,
};
use opentelemetry_otlp::{SpanExporter, WithExportConfig};
use opentelemetry_sdk::{propagation::TraceContextPropagator, trace::SdkTracerProvider, Resource};

const ENV_ENDPOINT: &str = "OTEL_EXPORTER_OTLP_ENDPOINT";

// The shim inherits Cubelet's environment, which may name the service "cubelet".
const SERVICE_NAME: &str = "cube-shim";

const SCOPE: &str = "github.com/tencentcloud/CubeSandbox/CubeShim";

const ATTR_SANDBOX_ID: &str = "cube.sandbox_id";

pub const SPAN_PREPARE_RESOURCE: &str = "cube-shim.prepare_resource";
pub const SPAN_PREPARE_SNAPSHOT: &str = "cube-shim.prepare_restore_snapshot";
pub const SPAN_LAUNCH_VMM: &str = "cube-shim.launch_vmm";
pub const SPAN_BOOT_VM: &str = "cube-shim.boot_vm";
pub const SPAN_RESTORE_VM: &str = "cube-shim.restore_vm";
pub const SPAN_AGENT_CONNECT: &str = "cube-shim.agent_connect";
pub const SPAN_GUEST_INIT: &str = "cube-shim.guest_init";
pub const SPAN_CONTAINER_CREATE: &str = "cube-shim.container_create";
pub const SPAN_CONTAINER_START: &str = "cube-shim.container_start";

// Error text can include credentials, so spans record only a generic status.
const STATUS_ERROR: &str = "error";
const STATUS_CANCELLED: &str = "cancelled";

#[derive(Clone, Default)]
pub struct Guard {
    provider: Option<SdkTracerProvider>,
}

impl Guard {
    pub fn shutdown(&self) {
        if let Some(provider) = self.provider.as_ref() {
            let _ = provider.shutdown();
        }
    }

    /// Containerd runs `start` and `delete` in short-lived processes that never serve a request.
    pub fn for_action(action: &str) -> Guard {
        Guard::from_endpoint(
            action,
            std::env::var(ENV_ENDPOINT).unwrap_or_default().trim(),
        )
    }

    fn from_endpoint(action: &str, endpoint: &str) -> Guard {
        if !action.is_empty() || endpoint.is_empty() {
            return Guard::default();
        }

        match build_provider(endpoint) {
            Ok(provider) => {
                global::set_text_map_propagator(TraceContextPropagator::new());
                global::set_tracer_provider(provider.clone());
                Guard {
                    provider: Some(provider),
                }
            }
            Err(err) => {
                log::warn!("telemetry setup failed, tracing disabled: {err}");
                Guard::default()
            }
        }
    }
}

fn build_provider(
    endpoint: &str,
) -> Result<SdkTracerProvider, Box<dyn std::error::Error + Send + Sync>> {
    let exporter = SpanExporter::builder()
        .with_tonic()
        .with_endpoint(endpoint)
        .build()?;

    Ok(SdkTracerProvider::builder()
        .with_batch_exporter(exporter)
        .with_resource(resource())
        .build())
}

/// Built without resource detectors, so nothing in the shim's inherited environment
/// can rename the service.
fn resource() -> Resource {
    Resource::builder_empty()
        .with_service_name(SERVICE_NAME.to_string())
        .build()
}

struct MetadataExtractor<'a>(&'a HashMap<String, Vec<String>>);

impl Extractor for MetadataExtractor<'_> {
    fn get(&self, key: &str) -> Option<&str> {
        self.0
            .get(key)
            .and_then(|values| values.first())
            .map(String::as_str)
    }

    fn keys(&self) -> Vec<&str> {
        self.0.keys().map(String::as_str).collect()
    }
}

pub struct Trace {
    parent: Option<Context>,
    sandbox: String,
}

impl Trace {
    /// A caller without a valid traceparent records nothing.
    pub fn extract(metadata: &HashMap<String, Vec<String>>, sandbox: &str) -> Self {
        let parent = global::get_text_map_propagator(|p| p.extract(&MetadataExtractor(metadata)));
        let parent = parent.span().span_context().is_valid().then_some(parent);
        Trace {
            parent,
            sandbox: sandbox.to_string(),
        }
    }

    pub fn start(&self, name: &'static str) -> Stage {
        let span = self.parent.as_ref().map(|parent| {
            let tracer = global::tracer(SCOPE);
            tracer
                .span_builder(name)
                .with_kind(SpanKind::Internal)
                .with_attributes([KeyValue::new(ATTR_SANDBOX_ID, self.sandbox.clone())])
                .start_with_context(&tracer, parent)
        });
        Stage { span }
    }
}

pub struct Stage {
    span: Option<BoxedSpan>,
}

impl Stage {
    pub async fn run<T, E>(mut self, future: impl Future<Output = Result<T, E>>) -> Result<T, E> {
        let out = future.await;
        self.finish(out.is_err().then_some(STATUS_ERROR));
        out
    }

    fn finish(&mut self, status: Option<&'static str>) {
        if let Some(mut span) = self.span.take() {
            if let Some(text) = status {
                span.set_status(Status::error(text));
            }
            span.end();
        }
    }
}

impl Drop for Stage {
    fn drop(&mut self) {
        self.finish(Some(STATUS_CANCELLED));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};
    use std::task::Context as TaskContext;

    use opentelemetry::propagation::TextMapPropagator;
    use opentelemetry::trace::noop::NoopTextMapPropagator;
    use opentelemetry_sdk::error::OTelSdkResult;
    use opentelemetry_sdk::trace::{SpanData, SpanExporter};

    const INBOUND: &str = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";
    const INBOUND_TRACE_ID: &str = "4bf92f3577b34da6a3ce929d0e0e4736";
    const INBOUND_PARENT_ID: &str = "00f067aa0ba902b7";
    const SANDBOX_ID: &str = "sb-1";
    const ENDPOINT: &str = "http://127.0.0.1:4317";

    fn traced_call(traceparent: &str) -> HashMap<String, Vec<String>> {
        let mut metadata = HashMap::new();
        metadata.insert("traceparent".to_string(), vec![traceparent.to_string()]);
        metadata
    }

    fn task_context() -> TaskContext<'static> {
        TaskContext::from_waker(futures::task::noop_waker_ref())
    }

    #[derive(Debug, Clone, Default)]
    struct RecordingExporter(Arc<Mutex<Vec<SpanData>>>);

    impl RecordingExporter {
        fn finished(&self) -> Vec<SpanData> {
            self.0.lock().unwrap().clone()
        }
    }

    impl SpanExporter for RecordingExporter {
        fn export(
            &mut self,
            batch: Vec<SpanData>,
        ) -> futures::future::BoxFuture<'static, OTelSdkResult> {
            self.0.lock().unwrap().extend(batch);
            Box::pin(std::future::ready(Ok(())))
        }
    }

    #[test]
    fn traceparent_is_read_from_request_metadata() {
        let propagator = TraceContextPropagator::new();

        let cx = propagator.extract(&MetadataExtractor(&traced_call(INBOUND)));
        assert_eq!(
            cx.span().span_context().trace_id().to_string(),
            INBOUND_TRACE_ID
        );
        assert_eq!(
            cx.span().span_context().span_id().to_string(),
            INBOUND_PARENT_ID
        );

        for metadata in [HashMap::new(), traced_call("garbage")] {
            let cx = propagator.extract(&MetadataExtractor(&metadata));
            assert!(
                !cx.span().span_context().is_valid(),
                "a caller without a valid traceparent is not traced"
            );
        }
    }

    #[tokio::test]
    async fn tracing_is_off_without_an_endpoint_or_outside_the_serving_process() {
        assert!(
            build_provider(ENDPOINT).is_ok(),
            "an OTLP endpoint yields an exporter"
        );
        assert!(
            Guard::from_endpoint("", "").provider.is_none(),
            "a shim without an endpoint must not export"
        );
        assert!(
            Guard::from_endpoint("start", ENDPOINT).provider.is_none(),
            "an action process must not start an exporter"
        );
    }

    #[test]
    fn the_shim_reports_its_own_service_name() {
        assert!(
            resource()
                .iter()
                .any(|(k, v)| k.as_str() == "service.name" && v.to_string() == SERVICE_NAME),
            "the shim must export as {SERVICE_NAME}"
        );
    }

    #[tokio::test(flavor = "current_thread")]
    async fn stages_report_their_outcome_under_the_caller_span() {
        let exporter = RecordingExporter::default();
        global::set_text_map_propagator(TraceContextPropagator::new());
        global::set_tracer_provider(
            SdkTracerProvider::builder()
                .with_simple_exporter(exporter.clone())
                .build(),
        );

        let trace = Trace::extract(&traced_call(INBOUND), SANDBOX_ID);

        trace
            .start(SPAN_LAUNCH_VMM)
            .run(async { Ok::<(), String>(()) })
            .await
            .expect("a stage that succeeds must end ok");
        let failed = trace
            .start(SPAN_BOOT_VM)
            .run(async { Err::<(), String>("boom".to_string()) })
            .await;
        assert_eq!(failed, Err("boom".to_string()));

        // A polled stage dropped mid-flight was cancelled, not completed.
        let pending = std::future::pending::<Result<(), String>>();
        let mut cancelled = Box::pin(trace.start(SPAN_PREPARE_SNAPSHOT).run(pending));
        assert!(cancelled.as_mut().poll(&mut task_context()).is_pending());
        drop(cancelled);

        // An untraced caller records nothing, whether it sends no traceparent or a malformed one.
        for metadata in [HashMap::new(), traced_call("garbage")] {
            Trace::extract(&metadata, SANDBOX_ID)
                .start(SPAN_RESTORE_VM)
                .run(async { Ok::<(), String>(()) })
                .await
                .expect("a stage that succeeds must end ok");
        }

        // Two stages in flight on one thread must not nest under each other.
        let (connect, create) = tokio::join!(
            trace.start(SPAN_AGENT_CONNECT).run(async {
                tokio::task::yield_now().await;
                Ok::<(), String>(())
            }),
            trace.start(SPAN_CONTAINER_CREATE).run(async {
                tokio::task::yield_now().await;
                Ok::<(), String>(())
            }),
        );
        connect.expect("a stage that succeeds must end ok");
        create.expect("a stage that succeeds must end ok");

        let spans = exporter.finished();
        let names: Vec<&str> = spans.iter().map(|s| s.name.as_ref()).collect();
        assert_eq!(spans.len(), 5, "unexpected spans: {names:?}");
        assert_eq!(
            names.iter().filter(|n| **n == SPAN_RESTORE_VM).count(),
            0,
            "an untraced caller must not record stages"
        );

        for span in &spans {
            assert_eq!(span.span_kind, SpanKind::Internal);
            assert_eq!(
                span.span_context.trace_id().to_string(),
                INBOUND_TRACE_ID,
                "span {} lost the caller trace",
                span.name
            );
            assert_eq!(
                span.parent_span_id.to_string(),
                INBOUND_PARENT_ID,
                "span {} must hang off the caller span",
                span.name
            );
            assert!(span
                .attributes
                .contains(&KeyValue::new(ATTR_SANDBOX_ID, SANDBOX_ID)));
        }

        let status = |name: &str| {
            spans
                .iter()
                .find(|s| s.name.as_ref() == name)
                .unwrap_or_else(|| panic!("no {name} span"))
                .status
                .clone()
        };
        assert_eq!(status(SPAN_LAUNCH_VMM), Status::Unset);
        assert_eq!(status(SPAN_BOOT_VM), Status::error(STATUS_ERROR));
        assert_eq!(
            status(SPAN_PREPARE_SNAPSHOT),
            Status::error(STATUS_CANCELLED)
        );

        // A shim started without an endpoint keeps the no-op propagator: even a traced
        // call must record nothing.
        global::set_text_map_propagator(NoopTextMapPropagator::new());
        let untraced = Trace::extract(&traced_call(INBOUND), SANDBOX_ID);
        untraced
            .start(SPAN_CONTAINER_START)
            .run(async { Ok::<(), String>(()) })
            .await
            .expect("a stage that succeeds must end ok");
        assert_eq!(
            exporter.finished().len(),
            spans.len(),
            "a shim without a propagator must not record stages"
        );
    }
}

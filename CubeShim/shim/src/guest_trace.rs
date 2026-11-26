// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, UNIX_EPOCH};

use opentelemetry::trace::{
    SpanContext, SpanId, SpanKind, Status, TraceFlags, TraceId, TraceState,
};
use opentelemetry::{InstrumentationScope, KeyValue};
use opentelemetry_otlp::{SpanExporter, WithExportConfig};
use opentelemetry_sdk::trace::{SpanData, SpanExporter as _};
use opentelemetry_sdk::Resource;
use protoc::{agent, agent_ttrpc::AgentServiceClient, empty::Empty};
use tokio::task::JoinHandle;
use ttrpc::context;

const DEFAULT_POLL_INTERVAL_SECS: u64 = 15;
const POLL_TIMEOUT: Duration = Duration::from_millis(250);
const EXPORT_TIMEOUT: Duration = Duration::from_secs(3);
const MAX_BATCH: usize = 32;
const MAX_PENDING: usize = 256;
struct PendingSpan {
    trace_id: TraceId,
    parent_id: SpanId,
    operation: &'static str,
    registered_at: Instant,
}

static PENDING: Mutex<VecDeque<PendingSpan>> = Mutex::new(VecDeque::new());

/// Accept one bounded report for each lifecycle request initiated by this shim.
pub(crate) fn register_parent(parent: &SpanContext, operation: &'static str) -> bool {
    let Ok(mut pending) = PENDING.try_lock() else {
        return false;
    };
    if pending.len() == MAX_PENDING {
        pending.pop_front();
    }
    pending.push_back(PendingSpan {
        trace_id: parent.trace_id(),
        parent_id: parent.span_id(),
        operation,
        registered_at: Instant::now(),
    });
    true
}

fn take_parent(trace_id: TraceId, parent_id: SpanId, operation: &str) -> bool {
    let Ok(mut pending) = PENDING.try_lock() else {
        return false;
    };
    pending
        .iter()
        .position(|entry| {
            entry.trace_id == trace_id
                && entry.parent_id == parent_id
                && entry.operation == operation
        })
        .and_then(|index| pending.remove(index))
        .is_some()
}

pub(crate) fn enabled() -> bool {
    matches!(std::env::var("CUBE_GUEST_TRACE").as_deref(), Ok("1"))
        && crate::telemetry::configured_endpoint().is_some()
}

fn poll_interval(setting: &str) -> Duration {
    let seconds = setting
        .trim()
        .parse::<u64>()
        .ok()
        .filter(|seconds| (5..=300).contains(seconds))
        .unwrap_or(DEFAULT_POLL_INTERVAL_SECS);
    Duration::from_secs(seconds)
}

pub(crate) struct TracePuller(JoinHandle<()>);

impl TracePuller {
    pub(crate) fn spawn(
        client: Arc<tokio::sync::Mutex<AgentServiceClient>>,
        sandbox_id: String,
    ) -> Option<Self> {
        if !enabled() {
            return None;
        }
        let endpoint = crate::telemetry::configured_endpoint()?;
        let interval = poll_interval(
            &std::env::var("CUBE_GUEST_TRACE_POLL_INTERVAL_SECS").unwrap_or_default(),
        );
        // The request uses a clone of the existing client handle, so the
        // lifecycle mutex is never held while waiting for a trace response.
        Some(Self(tokio::spawn(async move {
            let mut exporter = match SpanExporter::builder()
                .with_tonic()
                .with_endpoint(endpoint)
                .with_timeout(EXPORT_TIMEOUT)
                .build()
            {
                Ok(exporter) => exporter,
                Err(err) => {
                    log::warn!("guest trace exporter unavailable: {err}");
                    return;
                }
            };
            exporter.set_resource(
                &Resource::builder_empty()
                    .with_service_name("cube-agent")
                    .build(),
            );
            loop {
                tokio::time::sleep(interval).await;
                if !PENDING.try_lock().is_ok_and(|mut pending| {
                    // Stop polling idle guests whose lifecycle RPCs yielded no span.
                    pending.retain(|entry| entry.registered_at.elapsed() <= interval * 3);
                    !pending.is_empty()
                }) {
                    continue;
                }
                let poll_client = match client.try_lock() {
                    Ok(client) => client.clone(),
                    Err(_) => continue,
                };
                let empty = Empty::new();
                let request = poll_client.poll_guest_spans(
                    context::with_timeout(POLL_TIMEOUT.as_nanos() as i64),
                    &empty,
                );
                let batch = match tokio::time::timeout(POLL_TIMEOUT, request).await {
                    Ok(Ok(batch)) => batch,
                    // Stop on old guests and failed connections. The ttrpc
                    // client retains timed-out requests until a response arrives.
                    _ => break,
                };
                let spans = batch
                    .spans
                    .iter()
                    .take(MAX_BATCH)
                    .filter_map(|span| decode_span(span, &sandbox_id))
                    .filter(|span| {
                        take_parent(
                            span.span_context.trace_id(),
                            span.parent_span_id,
                            &span.name,
                        )
                    })
                    .collect::<Vec<_>>();
                if !spans.is_empty() {
                    let _ = tokio::time::timeout(EXPORT_TIMEOUT, exporter.export(spans)).await;
                }
            }
        })))
    }
}

impl Drop for TracePuller {
    fn drop(&mut self) {
        self.0.abort();
    }
}

fn decode_span(span: &agent::GuestTraceSpan, sandbox_id: &str) -> Option<SpanData> {
    let name = match span.operation.as_str() {
        "create_sandbox" => "create_sandbox",
        "create_container" => "create_container",
        "start_container" => "start_container",
        _ => return None,
    };
    let trace_id = TraceId::from_bytes(span.trace_id.as_slice().try_into().ok()?);
    let span_id = SpanId::from_bytes(span.span_id.as_slice().try_into().ok()?);
    let parent = SpanId::from_bytes(span.parent_span_id.as_slice().try_into().ok()?);
    if trace_id == TraceId::INVALID
        || span_id == SpanId::INVALID
        || parent == SpanId::INVALID
        || parent == span_id
        || span.start_unix_nanos == 0
        || span.end_unix_nanos < span.start_unix_nanos
    {
        return None;
    }
    Some(SpanData {
        span_context: SpanContext::new(
            trace_id,
            span_id,
            TraceFlags::SAMPLED,
            false,
            TraceState::default(),
        ),
        parent_span_id: parent,
        span_kind: SpanKind::Server,
        name: name.into(),
        start_time: UNIX_EPOCH + Duration::from_nanos(span.start_unix_nanos),
        end_time: UNIX_EPOCH + Duration::from_nanos(span.end_unix_nanos),
        attributes: vec![KeyValue::new("cube.sandbox_id", sandbox_id.to_string())],
        dropped_attributes_count: 0,
        events: Default::default(),
        links: Default::default(),
        status: Status::Unset,
        instrumentation_scope: InstrumentationScope::builder("cube-agent").build(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn poll_interval_falls_back_to_the_default_for_invalid_values() {
        for input in ["", "0", "4", "301", "-5", "15s"] {
            assert_eq!(poll_interval(input), Duration::from_secs(15));
        }
        assert_eq!(poll_interval("5"), Duration::from_secs(5));
        assert_eq!(poll_interval(" 30 "), Duration::from_secs(30));
        assert_eq!(poll_interval("300"), Duration::from_secs(300));
    }

    #[test]
    fn accepts_only_one_valid_report_for_a_guest_rpc() {
        let trace_id = TraceId::from_bytes([1; 16]);
        let parent = SpanId::from_bytes([2; 8]);
        let context = SpanContext::new(
            trace_id,
            parent,
            TraceFlags::SAMPLED,
            false,
            TraceState::default(),
        );
        let mut report = agent::GuestTraceSpan {
            trace_id: vec![1; 16],
            span_id: vec![3; 8],
            parent_span_id: vec![2; 8],
            operation: "create_container".into(),
            start_unix_nanos: 100,
            end_unix_nanos: 200,
            ..Default::default()
        };
        let span = decode_span(&report, "sandbox-1").unwrap();
        assert_eq!(span.span_context.trace_id(), trace_id);
        assert_eq!(span.parent_span_id, parent);
        assert_eq!(span.span_kind, SpanKind::Server);
        assert_eq!(
            span.attributes,
            [KeyValue::new("cube.sandbox_id", "sandbox-1")]
        );
        assert!(!take_parent(trace_id, parent, &span.name));
        assert!(register_parent(&context, "create_container"));
        assert!(!take_parent(trace_id, parent, "start_container"));
        assert!(take_parent(trace_id, parent, &span.name));
        assert!(!take_parent(trace_id, parent, &span.name));

        report.operation = "unrecognized".into();
        assert!(decode_span(&report, "sandbox-1").is_none());
        report.operation = "create_container".into();
        report.end_unix_nanos = 99;
        assert!(decode_span(&report, "sandbox-1").is_none());
        report.end_unix_nanos = 200;
        report.span_id = report.parent_span_id.clone();
        assert!(decode_span(&report, "sandbox-1").is_none());
    }
}

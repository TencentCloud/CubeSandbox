// Copyright (c) 2020-2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::{HashMap, VecDeque};
use std::sync::{LazyLock, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use opentelemetry::propagation::TextMapPropagator;
use opentelemetry::sdk::export::trace::SpanData;
use opentelemetry::sdk::propagation::TraceContextPropagator;
use opentelemetry::sdk::trace::{Span, SpanProcessor, TracerProvider};
use opentelemetry::trace::{SpanKind, TraceContextExt, TraceResult, Tracer, TracerProvider as _};
use opentelemetry::Context;
use protocols::agent::{GuestTraceBatch, GuestTraceSpan};

const CAPACITY: usize = 256;
const BATCH_SIZE: usize = 32;
static QUEUE: Mutex<VecDeque<GuestTraceSpan>> = Mutex::new(VecDeque::new());
static PROVIDER: LazyLock<TracerProvider> = LazyLock::new(|| {
    TracerProvider::builder()
        .with_span_processor(QueueProcessor)
        .build()
});

#[derive(Debug)]
struct QueueProcessor;

impl SpanProcessor for QueueProcessor {
    fn on_start(&self, _: &Span, _: &Context) {}

    fn on_end(&self, span: SpanData) {
        if let Ok(mut queue) = QUEUE.try_lock() {
            if queue.len() == CAPACITY {
                queue.pop_front();
            }
            queue.push_back(GuestTraceSpan {
                trace_id: span
                    .span_context
                    .trace_id()
                    .to_u128()
                    .to_be_bytes()
                    .to_vec(),
                span_id: span.span_context.span_id().to_u64().to_be_bytes().to_vec(),
                parent_span_id: span.parent_span_id.to_u64().to_be_bytes().to_vec(),
                operation: span.name.into_owned(),
                start_unix_nanos: unix_nanos(span.start_time),
                end_unix_nanos: unix_nanos(span.end_time),
                ..Default::default()
            });
        }
    }

    fn force_flush(&self) -> TraceResult<()> {
        Ok(())
    }

    fn shutdown(&mut self) -> TraceResult<()> {
        Ok(())
    }
}

fn unix_nanos(time: SystemTime) -> u64 {
    time.duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|d| d.as_nanos().try_into().ok())
        .unwrap_or_default()
}

pub fn poll_spans() -> GuestTraceBatch {
    let spans = QUEUE
        .lock()
        .map(|mut queue| {
            let count = queue.len().min(BATCH_SIZE);
            queue.drain(..count).collect()
        })
        .unwrap_or_default();
    GuestTraceBatch {
        spans,
        ..Default::default()
    }
}

/// Instrument only Shim lifecycle calls that opted into guest collection.
/// Dropping the returned SDK span ends it without attaching RPC arguments.
pub fn start_rpc(
    metadata: &HashMap<String, Vec<String>>,
    name: &'static str,
) -> Option<opentelemetry::sdk::trace::Span> {
    if !metadata.get("cube-guest-trace")?.iter().any(|v| v == "1") {
        return None;
    }
    let headers: HashMap<_, _> = ["traceparent", "tracestate"]
        .into_iter()
        .filter_map(|key| {
            metadata
                .get(key)
                .and_then(|values| values.first())
                .map(|value| (key.to_string(), value.clone()))
        })
        .collect();
    let parent = TraceContextPropagator::new().extract(&headers);
    let parent_span = parent.span();
    let parent_context = parent_span.span_context();
    if !parent_context.is_valid() || !parent_context.is_sampled() {
        return None;
    }
    let tracer = PROVIDER.get_tracer("cube-agent", None);
    Some(
        tracer
            .span_builder(name)
            .with_kind(SpanKind::Server)
            .with_parent_context(parent)
            .start(&tracer),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn poll_drains_bounded_sampled_spans_with_the_shim_parent() {
        let mut metadata = HashMap::from([(
            "traceparent".to_string(),
            vec!["00-0123456789abcdef0123456789abcdef-0123456789abcdef-01".to_string()],
        )]);
        assert!(start_rpc(&metadata, "create_container").is_none());
        metadata.insert("cube-guest-trace".into(), vec!["1".into()]);
        metadata.get_mut("traceparent").unwrap()[0].replace_range(53..55, "00");
        assert!(start_rpc(&metadata, "create_container").is_none());
        metadata.get_mut("traceparent").unwrap()[0].replace_range(53..55, "01");

        for _ in 0..=CAPACITY {
            drop(start_rpc(&metadata, "create_container").unwrap());
        }
        for _ in 0..CAPACITY / BATCH_SIZE {
            let batch = poll_spans();
            assert_eq!(batch.spans.len(), BATCH_SIZE);
            for span in batch.spans {
                assert_eq!(
                    span.trace_id,
                    0x0123456789abcdef0123456789abcdefu128.to_be_bytes()
                );
                assert_eq!(span.parent_span_id, 0x0123456789abcdefu64.to_be_bytes());
                assert_eq!(span.operation, "create_container");
                assert!(span.start_unix_nanos > 0);
                assert!(span.end_unix_nanos >= span.start_unix_nanos);
            }
        }
        assert!(poll_spans().spans.is_empty());
    }
}

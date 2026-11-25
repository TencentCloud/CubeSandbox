// Copyright (c) 2020-2021 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

use anyhow::Result;
use opentelemetry::sdk::propagation::TraceContextPropagator;
use opentelemetry::{
    global,
    sdk::{trace::Config, Resource},
    trace::{TraceContextExt, TracerProvider},
    KeyValue,
};
use slog::{info, o, Logger};
use std::collections::HashMap;
use tracing_opentelemetry::OpenTelemetryLayer;
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::Registry;
use ttrpc::r#async::TtrpcContext;

pub fn setup_tracing(name: &'static str, logger: &Logger) -> Result<()> {
    let logger = logger.new(o!("subsystem" => "vsock-tracer"));

    let exporter = vsock_exporter::Exporter::builder()
        .with_logger(&logger)
        .init();

    let config =
        Config::default().with_resource(Resource::new(vec![KeyValue::new("service.name", name)]));

    let builder = opentelemetry::sdk::trace::TracerProvider::builder()
        .with_batch_exporter(exporter, opentelemetry::runtime::TokioCurrentThread)
        .with_config(config);

    let provider = builder.build();

    // We don't need a versioned tracer.
    let version = None;

    let tracer = provider.get_tracer(name, version);

    let _global_provider = global::set_tracer_provider(provider);

    let layer = OpenTelemetryLayer::new(tracer);

    let subscriber = Registry::default().with(layer);

    tracing::subscriber::set_global_default(subscriber)?;

    global::set_text_map_propagator(TraceContextPropagator::new());

    info!(logger, "tracing setup");

    Ok(())
}

pub fn end_tracing() {
    global::shutdown_tracer_provider();
}

pub fn extract_carrier_from_ttrpc(ttrpc_context: &TtrpcContext) -> HashMap<String, String> {
    let mut carrier = HashMap::new();
    for (k, v) in &ttrpc_context.metadata {
        carrier.insert(k.clone(), v.join(","));
    }

    carrier
}

pub fn has_traced_parent(context: &opentelemetry::Context) -> bool {
    context.span().span_context().is_valid()
}

#[macro_export]
macro_rules! trace_rpc_call {
    ($ctx: ident, $name:literal, $req: ident) => {
        // extract context from request context
        let parent_context = global::get_text_map_propagator(|propagator| {
            propagator.extract(&extract_carrier_from_ttrpc($ctx))
        });

        // Do not export payloads or start orphan traces for untraced RPCs.
        let rpc_span = crate::tracer::has_traced_parent(&parent_context).then(|| {
            let span = span!(tracing::Level::INFO, $name, "mod" = "rpc.rs");
            span.set_parent(parent_context);
            span
        });
        let _enter = rpc_span.as_ref().map(|span| span.enter());
    };
}

#[cfg(test)]
mod tests {
    use super::*;
    use opentelemetry::propagation::TextMapPropagator;

    #[test]
    fn requires_a_valid_inbound_traceparent() {
        let propagator = TraceContextPropagator::new();
        let mut carrier = HashMap::new();
        assert!(!has_traced_parent(&propagator.extract(&carrier)));
        carrier.insert("traceparent".to_string(), "invalid".to_string());
        assert!(!has_traced_parent(&propagator.extract(&carrier)));
        carrier.insert(
            "traceparent".to_string(),
            "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01".to_string(),
        );
        assert!(has_traced_parent(&propagator.extract(&carrier)));
    }
}

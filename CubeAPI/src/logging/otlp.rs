// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! OpenTelemetry OTLP log backend — stub.
//!
//! Exports log events as OTLP `LogRecord`s via gRPC or HTTP/protobuf to any
//! OpenTelemetry-compatible collector (e.g. the OTel Collector, Jaeger, Tempo).
//!
//! # TODO (wire-up checklist)
//!
//! 1. Add to `Cargo.toml`:
//!    ```toml
//!    opentelemetry = "0.21"
//!    opentelemetry-otlp = { version = "0.14", features = ["logs", "grpc-tonic"] }
//!    opentelemetry_sdk = { version = "0.21", features = ["logs", "rt-tokio"] }
//!    tracing-opentelemetry = "0.22"
//!    ```
//! 2. Initialise the OTLP exporter in `main.rs`:
//!    ```rust,no_run
//!    use opentelemetry_otlp::WithExportConfig;
//!    let exporter = opentelemetry_otlp::new_exporter()
//!        .tonic()
//!        .with_endpoint("http://otel-collector:4317");
//!    ```
//! 3. Construct `OtlpLogger` with the exporter and register in `MultiLogger`.
//!
//! # Why a custom Logger wrapper instead of tracing-subscriber?
//!
//! `tracing-subscriber` exports *traces* and *spans*.  Our `LogEvent` is a
//! structured application event (not a trace span), so we ship it directly as
//! an OTLP `LogRecord` to keep the two concerns separate.

use super::{LogEvent, Logger};
use async_trait::async_trait;

/// OpenTelemetry OTLP log backend (stub — not yet implemented).
#[derive(Clone, Default)]
pub struct OtlpLogger;

impl OtlpLogger {
    #[allow(dead_code)]
    pub fn new() -> Self {
        Self
    }
}

#[async_trait]
impl Logger for OtlpLogger {
    async fn log(&self, _event: LogEvent) {
        // TODO: convert LogEvent → opentelemetry_sdk::logs::LogRecord
        // and emit via the configured OTLP log exporter.
        //
        // Sketch:
        //   let record = LogRecord::builder()
        //       .with_timestamp(_event.timestamp)
        //       .with_severity_text(_event.level.to_string())
        //       .with_body(AnyValue::String(_event.event.into()))
        //       .build();
        //   self.provider.emit(record);
    }

    async fn flush(&self) {
        // TODO: self.provider.force_flush()
    }

    fn name(&self) -> &'static str {
        "otlp"
    }
}

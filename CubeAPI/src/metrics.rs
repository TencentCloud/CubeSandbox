// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

use std::{fmt, sync::Arc, time::Duration};

use anyhow::Result;
use prometheus::{Encoder, HistogramVec, IntCounterVec, Opts, Registry, TextEncoder};

use crate::error::AppError;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BusinessOperation {
    SandboxCreate,
    SandboxDestroy,
}

impl BusinessOperation {
    pub fn as_str(self) -> &'static str {
        match self {
            BusinessOperation::SandboxCreate => "sandbox_create",
            BusinessOperation::SandboxDestroy => "sandbox_destroy",
        }
    }
}

impl fmt::Display for BusinessOperation {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BusinessResult {
    Success,
    Fail,
}

impl BusinessResult {
    pub fn as_str(self) -> &'static str {
        match self {
            BusinessResult::Success => "success",
            BusinessResult::Fail => "fail",
        }
    }
}

impl fmt::Display for BusinessResult {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

pub trait BusinessMetricsSink: Send + Sync {
    fn record_request(
        &self,
        operation: BusinessOperation,
        result: BusinessResult,
        code: i32,
        elapsed: Duration,
    );

    fn render_prometheus(&self) -> Result<String>;
}

#[derive(Clone)]
pub struct BusinessMetrics {
    registry: Registry,
    request_total: IntCounterVec,
    request_duration: HistogramVec,
}

impl BusinessMetrics {
    pub fn new() -> Result<Self> {
        let registry = Registry::new();
        let request_total = IntCounterVec::new(
            Opts::new(
                "cubeapi_business_request_total",
                "CubeAPI business request total by operation, result and code",
            ),
            &["operation", "result", "code"],
        )?;
        let request_duration = HistogramVec::new(
            prometheus::HistogramOpts::new(
                "cubeapi_business_request_duration_seconds",
                "CubeAPI business request duration in seconds by operation, result and code",
            )
            .buckets(prometheus::exponential_buckets(0.01, 2.0, 12)?),
            &["operation", "result", "code"],
        )?;

        registry.register(Box::new(request_total.clone()))?;
        registry.register(Box::new(request_duration.clone()))?;

        Ok(Self {
            registry,
            request_total,
            request_duration,
        })
    }

    pub fn record_request(
        &self,
        operation: BusinessOperation,
        result: BusinessResult,
        code: i32,
        elapsed: Duration,
    ) {
        let code = code.to_string();
        self.request_total
            .with_label_values(&[operation.as_str(), result.as_str(), &code])
            .inc();
        self.request_duration
            .with_label_values(&[operation.as_str(), result.as_str(), &code])
            .observe(elapsed.as_secs_f64());
    }

    pub fn render_prometheus(&self) -> Result<String> {
        let encoder = TextEncoder::new();
        let mut buffer = Vec::new();
        let mf = self.registry.gather();
        encoder.encode(&mf, &mut buffer)?;
        Ok(String::from_utf8(buffer)?)
    }
}

impl BusinessMetricsSink for BusinessMetrics {
    fn record_request(
        &self,
        operation: BusinessOperation,
        result: BusinessResult,
        code: i32,
        elapsed: Duration,
    ) {
        Self::record_request(self, operation, result, code, elapsed);
    }

    fn render_prometheus(&self) -> Result<String> {
        Self::render_prometheus(self)
    }
}

pub fn app_error_code(error: &AppError) -> i32 {
    match error {
        AppError::NotFound(_) => 404,
        AppError::Unauthorized(_) => 401,
        AppError::BadRequest(_) => 400,
        AppError::Internal(_) => 500,
        AppError::Conflict(_) => 409,
        AppError::ServiceUnavailable { .. } => 503,
        AppError::TooManyRequests(_) => 429,
        AppError::NotImplemented(_) => 501,
    }
}

pub async fn record_business_call<T, F>(
    sink: Arc<dyn BusinessMetricsSink>,
    operation: BusinessOperation,
    fut: F,
) -> Result<T, AppError>
where
    F: std::future::Future<Output = Result<T, AppError>>,
{
    let started = std::time::Instant::now();
    let result = fut.await;
    let elapsed = started.elapsed();

    match &result {
        Ok(_) => sink.record_request(operation, BusinessResult::Success, 0, elapsed),
        Err(err) => sink.record_request(
            operation,
            BusinessResult::Fail,
            app_error_code(err),
            elapsed,
        ),
    }

    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{sync::Mutex as StdMutex, time::Duration};

    #[derive(Default)]
    struct RecordingBusinessMetricsSink {
        records: StdMutex<Vec<(String, String, i32)>>,
    }

    impl RecordingBusinessMetricsSink {
        fn records(&self) -> Vec<(String, String, i32)> {
            self.records.lock().expect("lock records").clone()
        }
    }

    impl BusinessMetricsSink for RecordingBusinessMetricsSink {
        fn record_request(
            &self,
            operation: BusinessOperation,
            result: BusinessResult,
            code: i32,
            _elapsed: Duration,
        ) {
            self.records.lock().expect("lock records").push((
                operation.as_str().to_string(),
                result.as_str().to_string(),
                code,
            ));
        }

        fn render_prometheus(&self) -> Result<String> {
            Ok(String::new())
        }
    }

    #[test]
    fn business_metrics_render_success_rate_qps_and_latency() {
        let metrics = BusinessMetrics::new().expect("registry should build");

        metrics.record_request(
            BusinessOperation::SandboxCreate,
            BusinessResult::Success,
            0,
            Duration::from_millis(120),
        );
        metrics.record_request(
            BusinessOperation::SandboxDestroy,
            BusinessResult::Fail,
            500,
            Duration::from_millis(980),
        );

        let body = metrics.render_prometheus().expect("should render");
        assert!(body.contains(
            r#"cubeapi_business_request_total{code="0",operation="sandbox_create",result="success"} 1"#
        ));
        assert!(body.contains(
            r#"cubeapi_business_request_total{code="500",operation="sandbox_destroy",result="fail"} 1"#
        ));
        assert!(body.contains("cubeapi_business_request_duration_seconds_bucket"));
        assert!(body.contains("cubeapi_business_request_duration_seconds_sum"));
        assert!(body.contains("cubeapi_business_request_duration_seconds_count"));
    }

    #[test]
    fn app_error_code_maps_known_variants() {
        assert_eq!(app_error_code(&AppError::BadRequest("bad".into())), 400);
        assert_eq!(app_error_code(&AppError::NotFound("missing".into())), 404);
        assert_eq!(app_error_code(&AppError::Conflict("busy".into())), 409);
        assert_eq!(
            app_error_code(&AppError::Internal(anyhow::anyhow!("boom"))),
            500
        );
    }

    #[tokio::test]
    async fn record_business_call_captures_success_and_failure() {
        let sink = Arc::new(RecordingBusinessMetricsSink::default());

        let ok = record_business_call(sink.clone(), BusinessOperation::SandboxCreate, async {
            Ok::<(), AppError>(())
        })
        .await;
        assert!(ok.is_ok());

        let err = record_business_call(sink.clone(), BusinessOperation::SandboxDestroy, async {
            Err::<(), AppError>(AppError::Conflict("busy".into()))
        })
        .await;
        assert!(matches!(err, Err(AppError::Conflict(_))));

        let records = sink.records();
        assert_eq!(records.len(), 2);
        assert_eq!(records[0].0, "sandbox_create");
        assert_eq!(records[0].1, "success");
        assert_eq!(records[0].2, 0);
        assert_eq!(records[1].0, "sandbox_destroy");
        assert_eq!(records[1].1, "fail");
        assert_eq!(records[1].2, 409);
    }
}

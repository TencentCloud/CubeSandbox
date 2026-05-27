// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Structured logging configuration via tracing
//
// Log levels:
//   ERROR: Unrecoverable failures
//   WARN:  Recoverable issues
//   INFO:  Lifecycle events
//   DEBUG: Operation details
//   TRACE: Hot-path details

use tracing_subscriber::fmt::time::FormatTime;
use tracing_subscriber::{fmt, EnvFilter};

use crate::config::LogConfig;
use crate::pkg::errors::CubecowResult;

/// A custom timer that formats timestamps in local time using chrono.
///
/// Output format: `2026-04-02 14:22:46.123`
struct LocalTimer;

impl FormatTime for LocalTimer {
    fn format_time(&self, w: &mut fmt::format::Writer<'_>) -> std::fmt::Result {
        write!(
            w,
            "{}",
            chrono::Local::now().format("%Y-%m-%d %H:%M:%S%.3f")
        )
    }
}

/// Initialize the global tracing subscriber based on configuration.
///
/// Supports three output formats:
/// - `"json"`: Machine-readable JSON output (single-line)
/// - `"compact"` (default): Human-readable colored single-line output
/// - `"pretty"`: Human-readable colored multi-line output
///
/// When `config.file` is set, logs are written to the specified file path
/// with automatic daily rotation. Otherwise, logs are written to stdout.
pub fn init_logging(config: &LogConfig) -> CubecowResult<()> {
    let filter = EnvFilter::try_new(&config.level).unwrap_or_else(|_| {
        tracing::warn!(
            level = %config.level,
            "invalid log level, falling back to 'info'"
        );
        EnvFilter::new("info")
    });

    if let Some(ref file_path) = config.file {
        // Separate directory and filename for tracing-appender
        let directory = file_path
            .parent()
            .unwrap_or_else(|| std::path::Path::new("."));
        let file_name = file_path
            .file_name()
            .and_then(|n| n.to_str())
            .unwrap_or("cubecow.log");

        let file_appender = match config.rotation.as_str() {
            "hourly" => tracing_appender::rolling::hourly(directory, file_name),
            "never" => tracing_appender::rolling::never(directory, file_name),
            // "daily" or any other value defaults to daily rotation
            _ => tracing_appender::rolling::daily(directory, file_name),
        };
        let (non_blocking, _guard) = tracing_appender::non_blocking(file_appender);

        // Leak the guard so it lives for the entire process lifetime.
        // This is intentional — dropping the guard would stop log flushing.
        std::mem::forget(_guard);

        match config.format.as_str() {
            "json" => {
                fmt()
                    .json()
                    .with_timer(LocalTimer)
                    .with_env_filter(filter)
                    .with_target(true)
                    .with_thread_ids(true)
                    .with_file(true)
                    .with_line_number(true)
                    .with_ansi(false)
                    .with_writer(non_blocking)
                    .init();
            }
            "pretty" => {
                fmt()
                    .pretty()
                    .with_timer(LocalTimer)
                    .with_env_filter(filter)
                    .with_target(true)
                    .with_thread_ids(true)
                    .with_ansi(false)
                    .with_writer(non_blocking)
                    .init();
            }
            _ => {
                fmt()
                    .compact()
                    .with_timer(LocalTimer)
                    .with_env_filter(filter)
                    .with_target(true)
                    .with_thread_ids(true)
                    .with_ansi(false)
                    .with_writer(non_blocking)
                    .init();
            }
        }
    } else {
        // No file configured — write to stdout
        match config.format.as_str() {
            "json" => {
                fmt()
                    .json()
                    .with_timer(LocalTimer)
                    .with_env_filter(filter)
                    .with_target(true)
                    .with_thread_ids(true)
                    .with_file(true)
                    .with_line_number(true)
                    .init();
            }
            "pretty" => {
                fmt()
                    .pretty()
                    .with_timer(LocalTimer)
                    .with_env_filter(filter)
                    .with_target(true)
                    .with_thread_ids(true)
                    .init();
            }
            _ => {
                // "compact" or any other value defaults to compact single-line output
                fmt()
                    .compact()
                    .with_timer(LocalTimer)
                    .with_env_filter(filter)
                    .with_target(true)
                    .with_thread_ids(true)
                    .init();
            }
        }
    }

    tracing::info!(
        level = %config.level,
        format = %config.format,
        file = ?config.file,
        rotation = %config.rotation,
        "logging initialized"
    );

    Ok(())
}

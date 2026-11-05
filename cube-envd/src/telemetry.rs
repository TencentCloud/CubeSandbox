// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use crate::cli::LogFormat;
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn redact_does_not_leak_content() {
        let secret = "super-secret-token-value";
        let redacted = redact_secret(secret);
        assert!(!redacted.contains("super-secret"));
        assert!(redacted.contains("len="));
    }
}

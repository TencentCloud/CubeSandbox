// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

use crate::cli::LogFormat;
use tracing_subscriber::{fmt, prelude::*, EnvFilter};

pub fn init(format: LogFormat) {
    let filter = EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info"));
    match format {
        LogFormat::Text => tracing_subscriber::registry()
            .with(filter)
            .with(fmt::layer())
            .init(),
        LogFormat::Json => tracing_subscriber::registry()
            .with(filter)
            .with(fmt::layer().json())
            .init(),
    }
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

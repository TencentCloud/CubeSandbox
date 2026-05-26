// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Unified error types for cubecow
//
// All modules propagate errors through the `CubecowError` enum.
// Production code paths must never use `unwrap()`.

use std::io;

/// Unified error type for the entire cubecow system.
#[derive(Debug, thiserror::Error)]
pub enum CubecowError {
    /// Resource not found (volume, snapshot, etc.)
    #[error("not found: {0}")]
    NotFound(String),

    /// Resource already exists (duplicate name, id, etc.)
    #[error("already exists: {0}")]
    AlreadyExists(String),

    /// Resource exhausted (filesystem capacity, etc.)
    #[error("resource exhausted: {0}")]
    ResourceExhausted(String),

    /// Invalid argument provided by the caller
    #[error("invalid argument: {0}")]
    InvalidArg(String),

    /// IO error from the underlying system
    #[error("io error: {0}")]
    IoError(#[from] io::Error),

    /// Configuration loading or parsing error
    #[error("config error: {0}")]
    ConfigError(String),

    /// Precondition not met (e.g., volume has active snapshots)
    #[error("precondition failed: {0}")]
    PreconditionFailed(String),
}

/// Convenient type alias used throughout the codebase.
pub type CubecowResult<T> = Result<T, CubecowError>;

// ---------------------------------------------------------------------------
// From conversions: sub-module errors → CubecowError
// ---------------------------------------------------------------------------

impl From<toml::de::Error> for CubecowError {
    fn from(e: toml::de::Error) -> Self {
        CubecowError::ConfigError(e.to_string())
    }
}

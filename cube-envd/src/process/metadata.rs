// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Synchronization for process termination-cause metadata.

use std::sync::{Arc, Mutex, PoisonError};

/// Hold the termination marker across the kill operation. The output pump
/// takes the same lock before decorating `EndEvent`, so a successful user or
/// timeout kill cannot race with terminal metadata publication. Failed kills
/// clear the tentative marker and remain natural exits.
pub(crate) fn with_cause<T, F>(
    termination: &Arc<Mutex<Option<String>>>,
    cause: &str,
    action: F,
) -> std::io::Result<T>
where
    F: FnOnce() -> std::io::Result<T>,
{
    let mut marker = termination.lock().unwrap_or_else(PoisonError::into_inner);
    *marker = Some(cause.to_string());
    let result = action();
    if result.is_err() {
        *marker = None;
    }
    result
}

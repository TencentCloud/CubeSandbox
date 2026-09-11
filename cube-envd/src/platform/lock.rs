// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Poisoned-lock recovery policy, shared by the two process-wide state holders
//! (`platform::config` and `process::table`).
//!
//! A single handler panicking while holding one of these locks must not brick
//! the daemon for every later request (issue #1227: no silent failure, but also
//! no cascading death). The guarded data is a plain map/option with no
//! cross-field invariant a half-finished write could corrupt, so taking the
//! inner guard is safe.

use std::sync::{Mutex, MutexGuard, PoisonError, RwLock, RwLockReadGuard, RwLockWriteGuard};

pub(crate) fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
}

pub(crate) fn read<T>(l: &RwLock<T>) -> RwLockReadGuard<'_, T> {
    l.read().unwrap_or_else(PoisonError::into_inner)
}

pub(crate) fn write<T>(l: &RwLock<T>) -> RwLockWriteGuard<'_, T> {
    l.write().unwrap_or_else(PoisonError::into_inner)
}

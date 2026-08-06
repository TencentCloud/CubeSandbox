// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Shared daemon state: /init-injected env vars, optional access token and
//! the table of processes started through `process.Process/Start`.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Mutex, MutexGuard, PoisonError, RwLock, RwLockReadGuard, RwLockWriteGuard};

use crate::msg::process::ProcessConfig;

/// Recover a poisoned lock instead of propagating the panic. A single handler
/// panicking while holding one of these locks must not brick the daemon for
/// every later request (#1227: no silent failure, but also no cascading death);
/// the guarded data is a plain map/option with no cross-field invariant that a
/// half-finished write could corrupt, so taking the inner guard is safe.
fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
}
fn read<T>(l: &RwLock<T>) -> RwLockReadGuard<'_, T> {
    l.read().unwrap_or_else(PoisonError::into_inner)
}
fn write<T>(l: &RwLock<T>) -> RwLockWriteGuard<'_, T> {
    l.write().unwrap_or_else(PoisonError::into_inner)
}

pub struct ProcEntry {
    pub pid: u32,
    pub tag: Option<String>,
    pub config: ProcessConfig,
}

/// Opaque, process-lifetime-unique key for a live process in the table.
/// Keying by this instead of by pid is what prevents a finished process's
/// cleanup from evicting a *different* process that the OS happened to give
/// the same recycled pid.
pub type ProcHandle = u64;

pub struct AppState {
    env_vars: RwLock<HashMap<String, String>>,
    access_token: RwLock<Option<String>>,
    /// Set once the first /init lands so `envd --version` probes and health
    /// checks are unaffected either way.
    pub initialized: AtomicBool,
    processes: Mutex<HashMap<ProcHandle, ProcEntry>>,
    next_handle: AtomicU64,
}

impl AppState {
    pub fn new() -> Self {
        let mut env_vars = HashMap::new();
        // Upstream envd exposes E2B_SANDBOX through /envs and command
        // environments; with -isnotfc (the only mode CubeSandbox runs) the
        // value is "false".
        env_vars.insert("E2B_SANDBOX".to_string(), "false".to_string());
        Self {
            env_vars: RwLock::new(env_vars),
            access_token: RwLock::new(None),
            initialized: AtomicBool::new(false),
            processes: Mutex::new(HashMap::new()),
            next_handle: AtomicU64::new(1),
        }
    }

    /// Merge (not replace) env vars — matches Go envd behavior observed in
    /// the baseline: repeated /init calls accumulate variables.
    pub fn merge_env_vars(&self, vars: HashMap<String, String>) {
        let mut guard = write(&self.env_vars);
        guard.extend(vars);
        self.initialized.store(true, Ordering::Relaxed);
    }

    pub fn env_vars(&self) -> HashMap<String, String> {
        read(&self.env_vars).clone()
    }

    pub fn set_access_token(&self, token: String) {
        *write(&self.access_token) = Some(token);
    }

    /// Returns Err(()) when a token has been configured via /init and the
    /// provided header value does not match. When no token was configured
    /// the check always passes (baseline: uninitialized envd ignores
    /// X-Access-Token entirely). The comparison is constant-time so a caller
    /// cannot recover the token byte-by-byte from response timing.
    pub fn check_access_token(&self, header: Option<&str>) -> Result<(), ()> {
        match read(&self.access_token).as_deref() {
            None => Ok(()),
            Some(expected) => match header {
                Some(got) if constant_time_eq(expected.as_bytes(), got.as_bytes()) => Ok(()),
                _ => Err(()),
            },
        }
    }

    pub fn insert_process(&self, entry: ProcEntry) -> ProcHandle {
        let handle = self.next_handle.fetch_add(1, Ordering::Relaxed);
        lock(&self.processes).insert(handle, entry);
        handle
    }

    /// Remove a process by the handle returned from `insert_process`. Keying
    /// on the handle (not the pid) means a finished process only ever evicts
    /// its own entry, even if the OS has already recycled its pid into a newer
    /// process recorded in the table.
    pub fn remove_process(&self, handle: ProcHandle) {
        lock(&self.processes).remove(&handle);
    }

    pub fn list_processes(&self) -> Vec<(u32, Option<String>, ProcessConfig)> {
        let guard = lock(&self.processes);
        let mut out: Vec<_> = guard
            .values()
            .map(|e| (e.pid, e.tag.clone(), e.config.clone()))
            .collect();
        out.sort_by_key(|(pid, _, _)| *pid);
        out
    }

    /// Resolve a flat ProcessSelector (pid or tag) to a live pid. When several
    /// live processes share a tag the most recently started one wins, matching
    /// how a caller reusing a tag would expect the latest to be addressed.
    pub fn find_pid(&self, pid: Option<u32>, tag: Option<&str>) -> Option<u32> {
        let guard = lock(&self.processes);
        if let Some(p) = pid {
            return guard.values().any(|e| e.pid == p).then_some(p);
        }
        if let Some(t) = tag {
            return guard
                .iter()
                .filter(|(_, e)| e.tag.as_deref() == Some(t))
                .max_by_key(|(handle, _)| **handle)
                .map(|(_, e)| e.pid);
        }
        None
    }
}

/// Length-aware constant-time byte comparison. Runs in time independent of
/// where the first mismatch is (the length check leaks only the token length,
/// not its contents), so token verification can't be turned into a timing
/// oracle. Small enough not to warrant a dependency.
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn init_env_vars_merge_not_replace() {
        let s = AppState::new();
        s.merge_env_vars(HashMap::from([("A".into(), "1".into())]));
        s.merge_env_vars(HashMap::from([("B".into(), "2".into())]));
        let vars = s.env_vars();
        assert_eq!(vars.get("A").map(String::as_str), Some("1"));
        assert_eq!(vars.get("B").map(String::as_str), Some("2"));
        assert_eq!(vars.get("E2B_SANDBOX").map(String::as_str), Some("false"));
    }

    #[test]
    fn access_token_semantics() {
        let s = AppState::new();
        // Uninitialized: any header (or none) passes.
        assert!(s.check_access_token(None).is_ok());
        assert!(s.check_access_token(Some("whatever")).is_ok());
        s.set_access_token("secret".into());
        assert!(s.check_access_token(Some("secret")).is_ok());
        assert!(s.check_access_token(Some("wrong")).is_err());
        // Length mismatch and prefix match both rejected.
        assert!(s.check_access_token(Some("secretx")).is_err());
        assert!(s.check_access_token(Some("sec")).is_err());
        assert!(s.check_access_token(None).is_err());
    }

    #[test]
    fn constant_time_eq_basics() {
        assert!(constant_time_eq(b"", b""));
        assert!(constant_time_eq(b"abc", b"abc"));
        assert!(!constant_time_eq(b"abc", b"abd"));
        assert!(!constant_time_eq(b"abc", b"ab"));
        assert!(!constant_time_eq(b"ab", b"abc"));
    }

    #[test]
    fn process_table_selectors() {
        let s = AppState::new();
        let h = s.insert_process(ProcEntry {
            pid: 42,
            tag: Some("t1".into()),
            config: ProcessConfig::default(),
        });
        assert_eq!(s.find_pid(Some(42), None), Some(42));
        assert_eq!(s.find_pid(None, Some("t1")), Some(42));
        assert_eq!(s.find_pid(Some(41), None), None);
        assert_eq!(s.find_pid(None, Some("nope")), None);
        s.remove_process(h);
        assert_eq!(s.find_pid(None, Some("t1")), None);
    }

    #[test]
    fn remove_by_handle_does_not_evict_recycled_pid() {
        // A finished process and a newer one share the same recycled pid; the
        // old one's cleanup must not evict the live entry.
        let s = AppState::new();
        let old = s.insert_process(ProcEntry {
            pid: 100,
            tag: Some("old".into()),
            config: ProcessConfig::default(),
        });
        let _new = s.insert_process(ProcEntry {
            pid: 100,
            tag: Some("new".into()),
            config: ProcessConfig::default(),
        });
        s.remove_process(old);
        // The pid is still live (owned by the newer process) and its tag wins.
        assert_eq!(s.find_pid(Some(100), None), Some(100));
        assert_eq!(s.find_pid(None, Some("new")), Some(100));
        assert_eq!(s.find_pid(None, Some("old")), None);
    }
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Process-wide configuration: /init-injected env vars, the default user and
//! working directory, the access token and the /init timestamp gate.
//!
//! Read by the app layer (/init writes it, the lifecycle endpoints read it) and
//! by the domains: the filesystem data plane resolves the default user/workdir
//! and gates on the token, and the process engine merges `env_vars()` into a
//! child's environment. It therefore sits below the domains, not in `app/`.
//!
//! Contract: upstream `internal/api` (`/init`, `/envs`) and
//! `execcontext.Defaults`; the timestamp gate mirrors `utils.AtomicMax`.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Mutex, RwLock, RwLockReadGuard};

use crate::platform::lock::{lock, read, write};

pub struct Config {
    env_vars: RwLock<HashMap<String, String>>,
    access_token: RwLock<Option<String>>,
    /// User assumed when a request names none. `/init`'s `defaultUser`
    /// overrides it; until then it mirrors upstream's compile-time constant
    /// "root", which `/init` only replaces when the field is present and
    /// non-empty.
    default_user: RwLock<String>,
    /// Working directory supplied by `/init` (`defaultWorkdir`). Upstream
    /// substitutes it only for an *empty* path
    /// (`execcontext.ResolveDefaultWorkdir`), so the file surface rarely sees
    /// it while `process.Start` without a cwd does.
    default_workdir: RwLock<Option<String>>,
    /// Nanosecond high-water mark of the `/init` timestamps that were applied
    /// (upstream `utils.AtomicMax`). Setting the system clock from it is
    /// deliberately NOT implemented — see the declared differences in
    /// `app/lifecycle.rs`.
    last_set_time: Mutex<i64>,
    /// Set once the first /init lands so `envd --version` probes and health
    /// checks are unaffected either way.
    pub initialized: AtomicBool,
}

impl Config {
    pub fn new() -> Self {
        let mut env_vars = HashMap::new();
        // Upstream envd exposes E2B_SANDBOX through /envs and command
        // environments; with -isnotfc (the only mode CubeSandbox runs) the
        // value is "false".
        env_vars.insert("E2B_SANDBOX".to_string(), "false".to_string());
        Self {
            env_vars: RwLock::new(env_vars),
            access_token: RwLock::new(None),
            default_user: RwLock::new(crate::platform::identity::DEFAULT_USER.to_string()),
            default_workdir: RwLock::new(None),
            last_set_time: Mutex::new(0),
            initialized: AtomicBool::new(false),
        }
    }

    /// Merge (not replace) env vars — matches the Go envd baseline: repeated
    /// /init calls accumulate variables.
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

    /// Borrow the configured token so `/init` can validate the token carried
    /// in its body without cloning the secret.
    pub fn access_token(&self) -> RwLockReadGuard<'_, Option<String>> {
        read(&self.access_token)
    }

    pub fn default_user(&self) -> String {
        read(&self.default_user).clone()
    }

    pub fn default_workdir(&self) -> Option<String> {
        read(&self.default_workdir).clone()
    }

    /// Apply `/init`'s `defaultUser` / `defaultWorkdir`. Upstream ignores both
    /// when the field is absent *or* an empty string, so an empty value must
    /// not wipe the previous default.
    pub fn apply_init_defaults(&self, user: Option<&str>, workdir: Option<&str>) {
        if let Some(user) = user.filter(|u| !u.is_empty()) {
            *write(&self.default_user) = user.to_string();
        }
        if let Some(workdir) = workdir.filter(|w| !w.is_empty()) {
            *write(&self.default_workdir) = Some(workdir.to_string());
        }
    }

    /// `/init` timestamp gate (upstream `utils.AtomicMax.SetToGreater`):
    /// returns true when the request may update the state and raises the
    /// high-water mark. A request without a timestamp always proceeds.
    pub fn claim_timestamp(&self, incoming: Option<i64>) -> bool {
        let mut guard = lock(&self.last_set_time);
        if !timestamp_gate(*guard, incoming) {
            return false;
        }
        if let Some(nanos) = incoming {
            *guard = nanos;
        }
        true
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
}

/// `/init` timestamp comparison, mirroring `utils.AtomicMax.SetToGreater`
/// (an older request is dropped; an equal one passes and refreshes the mark).
/// This is protocol surface — a contract for retrying orchestrators — not a
/// local hot path: Cubelet never sends a timestamp at all (envVars-only).
pub fn timestamp_gate(prev_nanos: i64, incoming: Option<i64>) -> bool {
    match incoming {
        None => true,
        Some(nanos) => prev_nanos <= nanos,
    }
}

/// Length-aware constant-time byte comparison. Runs in time independent of
/// where the first mismatch is (the length check leaks only the token length,
/// not its contents), so token verification can't be turned into a timing
/// oracle. Small enough not to warrant a dependency.
pub(crate) fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
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
        let s = Config::new();
        s.merge_env_vars(HashMap::from([("A".into(), "1".into())]));
        s.merge_env_vars(HashMap::from([("B".into(), "2".into())]));
        let vars = s.env_vars();
        assert_eq!(vars.get("A").map(String::as_str), Some("1"));
        assert_eq!(vars.get("B").map(String::as_str), Some("2"));
        assert_eq!(vars.get("E2B_SANDBOX").map(String::as_str), Some("false"));
    }

    #[test]
    fn access_token_semantics() {
        let s = Config::new();
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
    fn timestamp_gate_matches_atomic_max() {
        // Upstream utils.AtomicMax.SetToGreater: older is rejected, an equal
        // timestamp passes (and is stored again).
        assert!(timestamp_gate(0, Some(1)));
        assert!(timestamp_gate(10, Some(10)));
        assert!(!timestamp_gate(10, Some(9)));
        // No timestamp at all: /init always applies its data.
        assert!(timestamp_gate(10, None));
    }

    #[test]
    fn init_defaults_ignore_absent_and_empty() {
        let s = Config::new();
        assert_eq!(s.default_user(), "root");
        assert_eq!(s.default_workdir(), None);
        // Empty strings must not wipe the previous default.
        s.apply_init_defaults(Some(""), Some(""));
        assert_eq!(s.default_user(), "root");
        assert_eq!(s.default_workdir(), None);
        // Absent fields are no-ops as well.
        s.apply_init_defaults(None, None);
        assert_eq!(s.default_user(), "root");
        // Non-empty values take effect and survive later empty/absent ones.
        s.apply_init_defaults(Some("user"), Some("/home/user"));
        assert_eq!(s.default_user(), "user");
        assert_eq!(s.default_workdir().as_deref(), Some("/home/user"));
        s.apply_init_defaults(Some(""), None);
        assert_eq!(s.default_user(), "user");
        assert_eq!(s.default_workdir().as_deref(), Some("/home/user"));
    }

    #[test]
    fn claim_timestamp_tracks_high_water_mark() {
        let s = Config::new();
        // First /init with any timestamp wins (the mark starts at 0).
        assert!(s.claim_timestamp(Some(1000)));
        // Same timestamp: allowed again (upstream SetToGreater semantics).
        assert!(s.claim_timestamp(Some(1000)));
        // Older: dropped — and the state must stay untouched.
        assert!(!s.claim_timestamp(Some(999)));
        // Newer: applied, raising the mark.
        assert!(s.claim_timestamp(Some(1001)));
        assert!(!s.claim_timestamp(Some(1000)));
        // No timestamp: always applied, without moving the mark.
        assert!(s.claim_timestamp(None));
        assert!(s.claim_timestamp(Some(1001)));
    }

    #[test]
    fn constant_time_eq_basics() {
        assert!(constant_time_eq(b"", b""));
        assert!(constant_time_eq(b"abc", b"abc"));
        assert!(!constant_time_eq(b"abc", b"abd"));
        assert!(!constant_time_eq(b"abc", b"ab"));
        assert!(!constant_time_eq(b"ab", b"abc"));
    }
}

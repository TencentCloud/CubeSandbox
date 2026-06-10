// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//! Build registry — keeps E2B-compatible per-build context in memory.
//!
//! When a client invokes the E2B-style `POST /templates`, we allocate a fresh
//! `(templateID, buildID)` pair and remember:
//!
//!   - the create request snapshot (so `POST /templates/{tid}/builds/{bid}`
//!     can resolve into the actual CubeMaster pipeline),
//!   - the docker-push registry credentials we just handed back to the client,
//!   - an append-only log buffer so the polling-based `?logsOffset=N` protocol
//!     keeps working,
//!   - the CubeMaster `jobID` once the build is dispatched, used by every
//!     subsequent status / logs lookup.
//!
//! ## Eviction
//!
//! The registry is bounded by **two complementary policies** so a long-running
//! CubeAPI process can't accumulate completed builds forever:
//!
//! 1. **TTL on terminal builds** — when a build transitions into
//!    `BuildStage::Ready` / `BuildStage::Error`, we stamp `terminal_at` and
//!    push it onto an ordered FIFO. A background tokio task wakes up every
//!    `gc_interval` and pops everything past `terminal_ttl`. In-flight builds
//!    (`WaitingPush`, `Building`) are never evicted by TTL.
//!
//! 2. **Hard size cap** — `create()` checks the cap and synchronously evicts
//!    the oldest terminal builds FIFO until the live count is at or below the
//!    cap. If every entry is still in-flight, we log a warning and let the
//!    cap be exceeded rather than killing an active build mid-flight.
//!
//! Both knobs come from `ServerConfig::build_registry_*` and default to
//! `(ttl=1h, cap=5000, gc_interval=5min)`. Setting any of them to `0`
//! disables that specific protection.
//!
//! Restart of CubeAPI invalidates inflight builds. This is acceptable for a
//! build flow that always reaches a terminal state within minutes — durable
//! persistence can be added later as a separate storage trait without
//! changing the call sites.

use chrono::{DateTime, Duration, Utc};
use dashmap::DashMap;
use std::collections::VecDeque;
use std::sync::{Arc, Mutex};
use std::time::Duration as StdDuration;
use uuid::Uuid;

use crate::models::{CreateTemplateRequest, RegistryCredential};

/// Lifecycle stage as understood by the E2B CLI.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BuildStage {
    /// Initial state: template has been registered, push credentials issued,
    /// waiting for the client to upload the image.
    WaitingPush,
    /// Image has been pushed; CubeMaster pipeline is running.
    Building,
    /// Image-build pipeline finished successfully.
    Ready,
    /// Image-build pipeline failed.
    Error,
}

impl BuildStage {
    pub fn as_str(self) -> &'static str {
        match self {
            BuildStage::WaitingPush => "waiting",
            BuildStage::Building => "building",
            BuildStage::Ready => "ready",
            BuildStage::Error => "error",
        }
    }

    /// `Ready` and `Error` are absorbing states — the orchestrator pipeline
    /// will not move out of them. Used as the gate for TTL eviction.
    pub fn is_terminal(self) -> bool {
        matches!(self, BuildStage::Ready | BuildStage::Error)
    }
}

#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct BuildContext {
    pub template_id: String,
    pub build_id: String,
    /// Original create request — replayed when the client calls
    /// `POST /templates/{tid}/builds/{bid}`.
    pub create_request: Arc<CreateTemplateRequest>,
    /// Registry credentials issued at create time. Pull host (used by
    /// CubeMaster) is encoded into `image_ref` so the rest of the system
    /// stays oblivious of registry internals.
    pub credential: RegistryCredential,
    /// Image reference CubeMaster will pull from once the client has pushed.
    pub image_ref: String,
    /// Authoritative "the client has actually completed an OCI manifest
    /// PUT against `image_ref`" flag. Set **exclusively** by
    /// `TemplateService::mark_image_pushed` after both:
    ///
    ///   - the manifest's `repo` segment matches the one we minted at
    ///     create time (cross-check guarding against tag collisions), and
    ///   - the upstream registry returned a `2xx` for the PUT.
    ///
    /// Consumers (especially `v3_trigger_build`) MUST gate the
    /// "fall back to `ctx.image_ref` as the source image" branch on this
    /// field, not on `stage`. `image_ref` is *predicted* at create time
    /// and its non-emptiness alone does not prove anything was pushed;
    /// `stage` is also an indirect proxy that the v2/v3 dispatch paths
    /// mutate for unrelated reasons. This boolean is the only safe
    /// correctness signal.
    pub image_pushed: bool,
    /// CubeMaster `jobID` — empty until the build is actually dispatched.
    pub job_id: String,
    /// Append-only log lines (timestamps + plain message).
    pub logs: Vec<BuildLogLine>,
    pub stage: BuildStage,
    pub progress: i32,
    pub message: String,
    pub created_at: DateTime<Utc>,
    /// Wall-clock time at which `stage` first became terminal. `None` while
    /// the build is still in-flight. Drives the TTL-based eviction path.
    pub terminal_at: Option<DateTime<Utc>>,

    // ── V3 protocol-only metadata (populated by POST /v3/templates) ────────
    /// Template name (E2B `name`), e.g. "my-template" or "my-template:v1".
    pub name: String,
    /// Tag list assigned at create time; the trailing ":tag" of `name` is
    /// pre-pended into this list when present.
    pub tags: Vec<String>,
    /// CPU cores requested via E2B `cpuCount`.
    pub cpu_count: u32,
    /// Memory in MiB requested via E2B `memoryMB`.
    pub memory_mb: u32,
    /// Aliases list returned to the client (currently == [name without tag]).
    pub aliases: Vec<String>,
}

#[derive(Debug, Clone)]
pub struct BuildLogLine {
    pub timestamp: DateTime<Utc>,
    pub line: String,
}

#[derive(Debug, Clone, Copy)]
pub struct EvictionPolicy {
    /// How long a terminal build is kept after reaching Ready/Error.
    /// `None` disables TTL-based eviction.
    pub terminal_ttl: Option<Duration>,
    /// Hard cap on the number of distinct builds; `None` disables the cap.
    pub max_entries: Option<usize>,
    /// Background GC scan interval; `None` disables the background task
    /// (size-cap eviction at create-time still runs).
    pub gc_interval: Option<StdDuration>,
}

impl EvictionPolicy {
    pub fn from_config(cfg: &crate::config::ServerConfig) -> Self {
        Self {
            terminal_ttl: (cfg.build_registry_terminal_ttl_secs > 0)
                .then(|| Duration::seconds(cfg.build_registry_terminal_ttl_secs as i64)),
            max_entries: (cfg.build_registry_max_entries > 0)
                .then_some(cfg.build_registry_max_entries),
            gc_interval: (cfg.build_registry_gc_interval_secs > 0)
                .then(|| StdDuration::from_secs(cfg.build_registry_gc_interval_secs)),
        }
    }

    pub fn unbounded() -> Self {
        Self {
            terminal_ttl: None,
            max_entries: None,
            gc_interval: None,
        }
    }
}

/// One entry on the FIFO of terminal builds awaiting eviction. We keep the
/// `template_id` here so the GC path can clear *both* index keys
/// (`bid` and `tid::bid`) without a round-trip through the DashMap.
#[derive(Debug, Clone)]
struct TerminalEntry {
    build_id: String,
    template_id: String,
    terminal_at: DateTime<Utc>,
}

/// Thread-safe, in-process build registry.
#[derive(Clone)]
pub struct BuildRegistry {
    inner: Arc<DashMap<String, BuildContext>>,
    username_index: Arc<DashMap<String, String>>,
    terminal: Arc<Mutex<VecDeque<TerminalEntry>>>,
    policy: EvictionPolicy,
}

impl Default for BuildRegistry {
    fn default() -> Self {
        Self::with_policy(EvictionPolicy::unbounded())
    }
}

impl BuildRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn with_policy(policy: EvictionPolicy) -> Self {
        Self {
            inner: Arc::new(DashMap::new()),
            username_index: Arc::new(DashMap::new()),
            terminal: Arc::new(Mutex::new(VecDeque::new())),
            policy,
        }
    }

    /// Spawn the background TTL GC task. Idempotent in the sense that calling
    /// it twice will spawn two tasks — call exactly once from `AppServices`
    /// construction. Returns `None` when GC is disabled (`gc_interval = 0`),
    /// which is convenient for unit tests.
    pub fn spawn_gc(&self) -> Option<tokio::task::JoinHandle<()>> {
        let interval = self.policy.gc_interval?;
        let registry = self.clone();
        let handle = tokio::spawn(async move {
            let mut ticker = tokio::time::interval(interval);
            // Skip the immediate firing — let the process settle first.
            ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            ticker.tick().await;
            loop {
                ticker.tick().await;
                let evicted = registry.evict_expired(Utc::now());
                if evicted > 0 {
                    tracing::debug!(
                        evicted,
                        live = registry.inner.len(),
                        "build registry GC swept terminal builds"
                    );
                }
            }
        });
        Some(handle)
    }

    /// Register a brand-new build attempt. Returns the freshly allocated
    /// build_id alongside the stored context (cloned for read-only use by the
    /// caller).
    pub fn create(
        &self,
        template_id: String,
        request: CreateTemplateRequest,
        credential: RegistryCredential,
        image_ref: String,
    ) -> BuildContext {
        let build_id = format!("bld-{}", Uuid::new_v4().simple());
        let ctx = BuildContext {
            template_id: template_id.clone(),
            build_id: build_id.clone(),
            create_request: Arc::new(request),
            credential,
            image_ref,
            image_pushed: false,
            job_id: String::new(),
            logs: Vec::new(),
            stage: BuildStage::WaitingPush,
            progress: 0,
            message: "build registered, waiting for image push".to_string(),
            created_at: Utc::now(),
            terminal_at: None,
            name: String::new(),
            tags: Vec::new(),
            cpu_count: 0,
            memory_mb: 0,
            aliases: Vec::new(),
        };

        self.inner.insert(build_id.clone(), ctx.clone());
        self.inner.insert(compose_key(&template_id, &build_id), ctx.clone());

        let uname = ctx.credential.username.clone();
        if !uname.is_empty() {
            self.username_index.insert(uname, build_id.clone());
        }
        self.enforce_size_cap();

        ctx
    }

    pub fn find_by_registry_username(&self, username: &str) -> Option<BuildContext> {
        let bid = self.username_index.get(username)?.value().clone();
        self.get(&bid)
    }

    pub fn get(&self, build_id: &str) -> Option<BuildContext> {
        self.inner.get(build_id).map(|r| r.value().clone())
    }

    pub fn get_by_pair(&self, template_id: &str, build_id: &str) -> Option<BuildContext> {
        self.inner
            .get(&compose_key(template_id, build_id))
            .or_else(|| self.inner.get(build_id))
            .map(|r| r.value().clone())
    }

    /// Apply a mutation to a build context. Updates both index entries and,
    /// if the closure transitions the build into a terminal stage, stamps
    /// `terminal_at` and queues the build for TTL eviction.
    pub fn update<F>(&self, build_id: &str, mutate: F) -> Option<BuildContext>
    where
        F: FnOnce(&mut BuildContext),
    {
        let mut ctx = self.inner.get(build_id).map(|r| r.value().clone())?;
        let was_terminal = ctx.stage.is_terminal();
        mutate(&mut ctx);
        let now_terminal = ctx.stage.is_terminal();

        if !was_terminal && now_terminal {
            let stamp = Utc::now();
            ctx.terminal_at = Some(stamp);
            self.push_terminal(TerminalEntry {
                build_id: ctx.build_id.clone(),
                template_id: ctx.template_id.clone(),
                terminal_at: stamp,
            });
        } else if was_terminal && !now_terminal {
            ctx.terminal_at = None;
        }

        let pair_key = compose_key(&ctx.template_id, &ctx.build_id);
        self.inner.insert(build_id.to_string(), ctx.clone());
        self.inner.insert(pair_key, ctx.clone());
        Some(ctx)
    }

    pub fn append_log(&self, build_id: &str, line: impl Into<String>) {
        let line = line.into();
        self.update(build_id, |ctx| {
            ctx.logs.push(BuildLogLine {
                timestamp: Utc::now(),
                line,
            });
            const MAX_LOGS: usize = 10_000;
            if ctx.logs.len() > MAX_LOGS {
                let drop = ctx.logs.len() - MAX_LOGS;
                ctx.logs.drain(0..drop);
            }
        });
    }

    /// Drop every terminal build whose `terminal_at + ttl <= now`.
    ///
    /// Returns the number of *logical* builds (not index entries) removed.
    /// Exposed `pub(crate)` so tests can drive the GC deterministically
    /// without spinning up the background task.
    pub(crate) fn evict_expired(&self, now: DateTime<Utc>) -> usize {
        let Some(ttl) = self.policy.terminal_ttl else {
            return 0;
        };
        let cutoff = now - ttl;
        let mut removed = 0usize;

        loop {
            let entry = {
                let mut q = self.terminal.lock().expect("terminal queue poisoned");
                match q.front() {
                    Some(e) if e.terminal_at <= cutoff => q.pop_front().unwrap(),
                    _ => break,
                }
            };

            if self.try_evict_one(&entry) {
                removed += 1;
            }
        }

        removed
    }

    /// Drive the size cap. Intended to be called right after `create()`.
    /// Walks the terminal FIFO and evicts oldest entries until either the
    /// live build count is at or below `max_entries`, or the FIFO is empty.
    fn enforce_size_cap(&self) {
        let Some(cap) = self.policy.max_entries else {
            return;
        };
        let mut live = self.inner.len() / 2;
        if live <= cap {
            return;
        }

        loop {
            if live <= cap {
                return;
            }
            let entry = {
                let mut q = self.terminal.lock().expect("terminal queue poisoned");
                match q.pop_front() {
                    Some(e) => e,
                    None => break,
                }
            };
            if self.try_evict_one(&entry) {
                live = live.saturating_sub(1);
            }
        }

        if self.inner.len() / 2 > cap {
            tracing::warn!(
                cap,
                live = self.inner.len() / 2,
                "build registry exceeds max_entries but every remaining build is in-flight; \
                 not evicting active builds. Increase build_registry_max_entries or wait \
                 for in-flight builds to terminate."
            );
        }
    }

    /// Remove both index entries for one terminal build.
    /// Returns `true` if anything was actually removed.
    /// A `false` return covers two benign races:
    ///   - the build was already evicted (e.g. via duplicate FIFO entry),
    ///   - the build was un-set back to non-terminal (we refuse to drop
    ///     in-flight contexts here — TTL eviction is for terminal builds
    ///     only).
    fn try_evict_one(&self, entry: &TerminalEntry) -> bool {
        let still_terminal = self
            .inner
            .get(&entry.build_id)
            .map(|r| r.value().stage.is_terminal())
            .unwrap_or(false);
        if !still_terminal {
            return false;
        }
        let username = self
            .inner
            .get(&entry.build_id)
            .map(|r| r.value().credential.username.clone())
            .unwrap_or_default();
        let removed_bid = self.inner.remove(&entry.build_id).is_some();
        let removed_pair = self
            .inner
            .remove(&compose_key(&entry.template_id, &entry.build_id))
            .is_some();
        if !username.is_empty() {
            self.username_index
                .remove_if(&username, |_, v| v == &entry.build_id);
        }
        removed_bid || removed_pair
    }

    fn push_terminal(&self, entry: TerminalEntry) {
        if self.policy.terminal_ttl.is_none() && self.policy.max_entries.is_none() {
            return;
        }
        let mut q = self.terminal.lock().expect("terminal queue poisoned");
        q.push_back(entry);
    }

    #[cfg(test)]
    fn terminal_queue_len(&self) -> usize {
        self.terminal
            .lock()
            .expect("terminal queue poisoned")
            .len()
    }

    #[cfg(test)]
    pub(crate) fn live_count(&self) -> usize {
        self.inner.len() / 2
    }
}

fn compose_key(template_id: &str, build_id: &str) -> String {
    format!("{}::{}", template_id, build_id)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::CreateTemplateRequest;

    fn empty_request() -> CreateTemplateRequest {
        CreateTemplateRequest {
            template_id: String::new(),
            instance_type: None,
            alias: None,
            team_id: None,
            image: None,
            dockerfile: None,
            writable_layer_size: None,
            exposed_ports: None,
            probe_port: None,
            probe_path: None,
            cpu: None,
            memory: None,
            cpu_count: None,
            memory_mb: None,
            env: None,
            env_vars: None,
            allow_internet_access: None,
            network_type: None,
            nodes: None,
            registry_username: None,
            registry_password: None,
            command: None,
            args: None,
            dns: None,
            allow_out: None,
            deny_out: None,
            start_cmd: None,
            ready_cmd: None,
        }
    }

    fn empty_credential() -> RegistryCredential {
        RegistryCredential {
            url: "http://127.0.0.1:5000".to_string(),
            repository: "e2b/tpl".to_string(),
            username: "_token".to_string(),
            password: "secret".to_string(),
        }
    }

    fn make_registry(ttl_secs: i64, cap: usize) -> BuildRegistry {
        BuildRegistry::with_policy(EvictionPolicy {
            terminal_ttl: (ttl_secs > 0).then(|| Duration::seconds(ttl_secs)),
            max_entries: (cap > 0).then_some(cap),
            gc_interval: None,
        })
    }

    fn create_one(reg: &BuildRegistry, tid: &str) -> String {
        reg.create(
            tid.to_string(),
            empty_request(),
            empty_credential(),
            format!("127.0.0.1:5000/e2b/{}:bld", tid),
        )
        .build_id
    }

    fn mark_ready(reg: &BuildRegistry, bid: &str) {
        reg.update(bid, |c| c.stage = BuildStage::Ready)
            .expect("build present");
    }

    #[test]
    fn terminal_transition_stamps_terminal_at_and_enqueues() {
        let reg = make_registry(3600, 0);
        let bid = create_one(&reg, "tpl-a");
        assert_eq!(reg.terminal_queue_len(), 0);

        mark_ready(&reg, &bid);
        let ctx = reg.get(&bid).unwrap();
        assert!(ctx.terminal_at.is_some(), "terminal_at must be set");
        assert!(ctx.stage.is_terminal());
        assert_eq!(reg.terminal_queue_len(), 1);
    }

    #[test]
    fn duplicate_terminal_updates_do_not_grow_the_fifo() {
        let reg = make_registry(3600, 0);
        let bid = create_one(&reg, "tpl-a");
        mark_ready(&reg, &bid);
        for _ in 0..5 {
            reg.update(&bid, |c| c.message = "noise".to_string());
        }
        assert_eq!(
            reg.terminal_queue_len(),
            1,
            "FIFO must dedupe rising-edge transitions"
        );
    }

    #[test]
    fn evict_expired_drops_terminal_builds_past_ttl() {
        let reg = make_registry(60, 0);
        let bid_a = create_one(&reg, "tpl-a");
        let bid_b = create_one(&reg, "tpl-b");
        mark_ready(&reg, &bid_a);
        mark_ready(&reg, &bid_b);

        assert_eq!(reg.evict_expired(Utc::now() + Duration::seconds(30)), 0);
        assert_eq!(reg.live_count(), 2);

        let removed = reg.evict_expired(Utc::now() + Duration::seconds(120));
        assert_eq!(removed, 2);
        assert_eq!(reg.live_count(), 0);
        assert!(reg.get(&bid_a).is_none());
        assert!(reg.get(&bid_b).is_none());
        assert_eq!(reg.terminal_queue_len(), 0);
    }

    #[test]
    fn evict_expired_leaves_in_flight_builds_alone() {
        let reg = make_registry(60, 0);
        let bid_done = create_one(&reg, "tpl-done");
        let bid_live = create_one(&reg, "tpl-live");
        mark_ready(&reg, &bid_done);

        let removed = reg.evict_expired(Utc::now() + Duration::seconds(120));
        assert_eq!(removed, 1);
        assert!(reg.get(&bid_done).is_none(), "terminal build evicted");
        assert!(reg.get(&bid_live).is_some(), "in-flight build retained");
    }

    #[test]
    fn size_cap_evicts_oldest_terminal_first_at_create_time() {
        let reg = make_registry(0, 2); // cap = 2, no TTL.
        let bid_a = create_one(&reg, "tpl-a");
        let bid_b = create_one(&reg, "tpl-b");
        mark_ready(&reg, &bid_a);
        mark_ready(&reg, &bid_b);

        let _bid_c = create_one(&reg, "tpl-c");
        assert!(reg.get(&bid_a).is_none(), "oldest terminal evicted");
        assert!(reg.get(&bid_b).is_some());
        assert!(reg.live_count() <= 2);
    }

    #[test]
    fn size_cap_does_not_evict_active_builds() {
        let reg = make_registry(0, 1);
        let bid_live_1 = create_one(&reg, "tpl-x");
        let bid_live_2 = create_one(&reg, "tpl-y");
        assert!(reg.get(&bid_live_1).is_some());
        assert!(reg.get(&bid_live_2).is_some());
    }

    #[test]
    fn evict_expired_skips_builds_that_left_terminal_state() {
        let reg = make_registry(60, 0);
        let bid = create_one(&reg, "tpl-a");
        mark_ready(&reg, &bid);
        reg.update(&bid, |c| c.stage = BuildStage::Building);

        let removed = reg.evict_expired(Utc::now() + Duration::seconds(120));
        assert_eq!(removed, 0);
        assert!(reg.get(&bid).is_some());
    }

    #[test]
    fn unbounded_registry_does_not_queue_terminal_entries() {
        let reg = BuildRegistry::with_policy(EvictionPolicy::unbounded());
        let bid = create_one(&reg, "tpl-a");
        mark_ready(&reg, &bid);
        assert_eq!(reg.terminal_queue_len(), 0);
    }
}

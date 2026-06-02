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
//! The store is in-memory + bounded; restart of CubeAPI invalidates inflight
//! builds. This is acceptable for a build flow that always reaches a terminal
//! state within minutes — durable persistence can be added later as a separate
//! storage trait without changing the call sites.

use chrono::{DateTime, Utc};
use dashmap::DashMap;
use std::sync::Arc;
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
    /// CubeMaster `jobID` — empty until the build is actually dispatched.
    pub job_id: String,
    /// Append-only log lines (timestamps + plain message).
    pub logs: Vec<BuildLogLine>,
    pub stage: BuildStage,
    pub progress: i32,
    pub message: String,
    pub created_at: DateTime<Utc>,

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

/// Thread-safe, in-process build registry.
#[derive(Clone, Default)]
pub struct BuildRegistry {
    inner: Arc<DashMap<String, BuildContext>>,
}

impl BuildRegistry {
    pub fn new() -> Self {
        Self::default()
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
            job_id: String::new(),
            logs: Vec::new(),
            stage: BuildStage::WaitingPush,
            progress: 0,
            message: "build registered, waiting for image push".to_string(),
            created_at: Utc::now(),
            name: String::new(),
            tags: Vec::new(),
            cpu_count: 0,
            memory_mb: 0,
            aliases: Vec::new(),
        };

        // Index under both bid and (tid, bid) so lookups by either key work.
        self.inner.insert(build_id.clone(), ctx.clone());
        self.inner.insert(compose_key(&template_id, &build_id), ctx.clone());
        ctx
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

    /// Apply a mutation to a build context. Updates both index entries.
    pub fn update<F>(&self, build_id: &str, mutate: F) -> Option<BuildContext>
    where
        F: FnOnce(&mut BuildContext),
    {
        let mut ctx = self.inner.get(build_id).map(|r| r.value().clone())?;
        mutate(&mut ctx);

        let pair_key = compose_key(&ctx.template_id, &ctx.build_id);
        self.inner.insert(build_id.to_string(), ctx.clone());
        self.inner.insert(pair_key, ctx.clone());
        Some(ctx)
    }

    /// Append one log line. Truncates the head to bound memory at ~10k lines.
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
}

fn compose_key(template_id: &str, build_id: &str) -> String {
    format!("{}::{}", template_id, build_id)
}

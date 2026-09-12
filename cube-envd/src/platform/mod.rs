// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Shared facilities consumed by more than one domain, with no wire contract
//! of their own.
//!
//! Members: `identity` (request user/group resolution and path anchoring) and
//! `config` (env vars, default user/workdir, access token, /init timestamp
//! gate). Both are read from L3 domains, so they must sit below them.
//!
//! Not here: `cgroup/` is a single-domain resource boundary (process only) and
//! lives in `process/cgroup/`; `auth` is the only OS-facing surface today.
//!
//! Source: `auth.rs` (identity) and the config/token part of `state.rs`.

pub mod config;
pub mod identity;
pub mod lock;

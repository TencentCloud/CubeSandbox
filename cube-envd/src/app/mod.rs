// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Assembly layer: wiring and process-wide policy — the route table, the
//! transport pipeline, the shared-state composition root, the lifecycle
//! endpoints, the blocking-pool policy and the HTTP response adapters.
//!
//! No domain logic lives here: `filesystem/` and `process/` own their
//! contracts. This layer depends downward on both domains and is assembled by
//! `main.rs`.

pub mod cli;
pub mod handlers;
pub mod lifecycle;
pub mod metrics;
pub mod middleware;
pub mod pool;
pub mod routes;
pub mod state;

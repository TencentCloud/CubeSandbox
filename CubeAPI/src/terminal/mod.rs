// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Web Terminal module.
//!
//! Provides WebSocket-based interactive terminal access to sandbox containers.
//!
//! ## Architecture
//!
//! ```text
//! Browser (xterm.js) ←WSS→ CubeAPI ←HTTP→ CubeProxy ←HTTP→ envd (PTY)
//! ```
//!
//! The WebSocket terminates at CubeAPI, which proxies bidirectional I/O
//! to the envd `process.Process/Connect` streaming endpoint through
//! CubeProxy's existing sandbox routing.

pub mod proxy;
pub mod session;
pub mod ws_handler;

pub use ws_handler::{close_sessions_for_sandbox, TerminalState, DEFAULT_IDLE_TIMEOUT_SECS};
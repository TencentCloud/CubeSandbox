// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//! Web Terminal session lifecycle tracker.
//!
//! Each terminal session is an in-memory record that tracks the connection
//! between a browser WebSocket and an envd `process.Process/Connect` stream
//! inside a specific sandbox container. Sessions are ephemeral — they live
//! only as long as the CubeAPI process and are NOT persisted to Redis.

use dashmap::DashMap;
use std::sync::Arc;
use std::time::Instant;
use tokio::sync::watch;
use uuid::Uuid;

/// Unique identifier for a terminal session.
pub type SessionId = Uuid;

/// What caused a terminal session to end.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TerminalCloseReason {
    ClientDisconnect,
    IdleTimeout,
    SandboxPaused,
    SandboxDestroyed,
    ServerShutdown,
    AuthExpired,
    Error,
}

impl TerminalCloseReason {
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::ClientDisconnect => "client_disconnect",
            Self::IdleTimeout => "idle_timeout",
            Self::SandboxPaused => "sandbox_paused",
            Self::SandboxDestroyed => "sandbox_destroyed",
            Self::ServerShutdown => "server_shutdown",
            Self::AuthExpired => "auth_expired",
            Self::Error => "error",
        }
    }

    pub fn ws_close_reason(&self) -> &'static str {
        match self {
            Self::ClientDisconnect => "client disconnect",
            Self::IdleTimeout => "idle timeout",
            Self::SandboxPaused => "sandbox paused",
            Self::SandboxDestroyed => "sandbox destroyed",
            Self::ServerShutdown => "server shutdown",
            Self::AuthExpired => "auth expired",
            Self::Error => "error",
        }
    }
}

/// In-memory record of a single terminal session.
#[derive(Debug, Clone)]
pub struct TerminalSession {
    /// Unique session identifier (UUID v4).
    pub session_id: SessionId,
    /// The sandbox this session is connected to.
    pub sandbox_id: String,
    /// Target container name within the sandbox.
    pub container_name: String,
    /// Authenticated user identity (or "anonymous" when auth is disabled).
    pub user: String,
    /// Remote IP of the client.
    pub remote_addr: String,
    /// When the session was created.
    pub started_at: Instant,
    /// Last time any data flowed in either direction.
    pub last_activity_at: Instant,
    /// Seconds of inactivity before the session is forcefully closed.
    pub idle_timeout_secs: u64,
}

impl TerminalSession {
    pub fn new(
        sandbox_id: String,
        container_name: String,
        user: String,
        remote_addr: String,
        idle_timeout_secs: u64,
    ) -> Self {
        let now = Instant::now();
        Self {
            session_id: Uuid::new_v4(),
            sandbox_id,
            container_name,
            user,
            remote_addr,
            started_at: now,
            last_activity_at: now,
            idle_timeout_secs,
        }
    }

    pub fn touch(&mut self) {
        self.last_activity_at = Instant::now();
    }

    pub fn is_idle(&self) -> bool {
        self.last_activity_at.elapsed().as_secs() >= self.idle_timeout_secs
    }

    pub fn duration_ms(&self) -> u128 {
        self.started_at.elapsed().as_millis()
    }
}

/// Thread-safe registry of active terminal sessions.
///
/// Sessions are keyed by `SessionId` and indexed by `sandbox_id` for
/// bulk cleanup (e.g. when a sandbox is destroyed).
///
/// Maintains secondary O(1) counters for per-sandbox and per-user
/// session limits so connection handshakes avoid full-table scans.
#[derive(Clone)]
pub struct SessionTracker {
    sessions: Arc<DashMap<SessionId, TerminalSession>>,
    sandbox_counts: Arc<DashMap<String, usize>>,
    user_counts: Arc<DashMap<String, usize>>,
    /// Per-session shutdown signals. When a sandbox is paused or destroyed,
    /// `close_sessions_for_sandbox` sends `Some(reason)` through these
    /// channels so running tasks can send WS close frames before exiting.
    /// Initial value is `None` (no shutdown requested).
    shutdown_senders: Arc<DashMap<SessionId, watch::Sender<Option<TerminalCloseReason>>>>,
}

impl SessionTracker {
    pub fn new() -> Self {
        Self {
            sessions: Arc::new(DashMap::new()),
            sandbox_counts: Arc::new(DashMap::new()),
            user_counts: Arc::new(DashMap::new()),
            shutdown_senders: Arc::new(DashMap::new()),
        }
    }

    /// Register a new session and return its ID.
    pub fn create(&self, session: TerminalSession) -> SessionId {
        let id = session.session_id;
        // Increment O(1) counters.
        self.sandbox_counts
            .entry(session.sandbox_id.clone())
            .and_modify(|c| *c += 1)
            .or_insert(1);
        self.user_counts
            .entry(session.user.clone())
            .and_modify(|c| *c += 1)
            .or_insert(1);
        self.sessions.insert(id, session);
        id
    }

    /// Remove a session from the tracker. Returns the removed session if it existed.
    pub fn remove(&self, session_id: &SessionId) -> Option<TerminalSession> {
        let removed = self.sessions.remove(session_id).map(|(_, s)| s);
        if let Some(ref session) = removed {
            // Decrement O(1) counters.
            self.dec_counter(&self.sandbox_counts, &session.sandbox_id);
            self.dec_counter(&self.user_counts, &session.user);
        }
        removed
    }

    /// Decrement a counter, removing the entry when it reaches zero.
    fn dec_counter(&self, map: &DashMap<String, usize>, key: &str) {
        // Use remove_if_mut-like pattern: get_mut + check.
        // If not found or already 0, leave things consistent.
        if let Some(mut entry) = map.get_mut(key) {
            if *entry <= 1 {
                drop(entry);
                map.remove(key);
            } else {
                *entry -= 1;
            }
        }
    }

    /// Look up a session by ID.
    pub fn get(&self, session_id: &SessionId) -> Option<dashmap::mapref::one::Ref<'_, SessionId, TerminalSession>> {
        self.sessions.get(session_id)
    }

    /// List all active sessions for a given sandbox.
    pub fn list_by_sandbox(&self, sandbox_id: &str) -> Vec<SessionId> {
        self.sessions
            .iter()
            .filter(|entry| entry.sandbox_id == sandbox_id)
            .map(|entry| entry.session_id)
            .collect()
    }

    /// Remove and return all sessions for a given sandbox.
    ///
    /// Uses `retain()` for single-pass atomic-style removal within each
    /// shard instead of collect-then-remove.
    pub fn remove_by_sandbox(&self, sandbox_id: &str) -> Vec<TerminalSession> {
        let mut removed = Vec::new();
        self.sessions.retain(|_, session| {
            if session.sandbox_id == sandbox_id {
                self.dec_counter(&self.user_counts, &session.user);
                removed.push(session.clone());
                false
            } else {
                true
            }
        });
        // Remove the sandbox counter entirely — all sessions gone.
        self.sandbox_counts.remove(sandbox_id);
        removed
    }

    /// Count the number of active sessions for a given sandbox (O(1)).
    pub fn count_by_sandbox(&self, sandbox_id: &str) -> usize {
        self.sandbox_counts
            .get(sandbox_id)
            .map(|entry| *entry)
            .unwrap_or(0)
    }

    /// Count the number of active sessions for a given user (O(1)).
    pub fn count_by_user(&self, user: &str) -> usize {
        self.user_counts
            .get(user)
            .map(|entry| *entry)
            .unwrap_or(0)
    }

    /// Find all sessions that have exceeded their idle timeout.
    pub fn find_idle_sessions(&self) -> Vec<(SessionId, TerminalCloseReason)> {
        self.sessions
            .iter()
            .filter(|entry| entry.is_idle())
            .map(|entry| (entry.session_id, TerminalCloseReason::IdleTimeout))
            .collect()
    }

    /// Register a shutdown sender for a session. Called by
    /// `handle_terminal_socket` before entering the main event loop.
    pub fn register_shutdown_sender(
        &self,
        session_id: SessionId,
        sender: watch::Sender<Option<TerminalCloseReason>>,
    ) {
        self.shutdown_senders.insert(session_id, sender);
    }

    /// Remove the shutdown sender for a session. Called by
    /// `cleanup_session` to prevent stale entries.
    pub fn remove_shutdown_sender(&self, session_id: &SessionId) {
        self.shutdown_senders.remove(session_id);
    }

    /// Send a shutdown signal to all sessions for a given sandbox.
    /// Returns the session IDs that were signaled (before removal).
    /// Used by `close_sessions_for_sandbox` before bulk removal.
    pub fn signal_shutdown_for_sandbox(
        &self,
        sandbox_id: &str,
        reason: TerminalCloseReason,
    ) -> Vec<SessionId> {
        let session_ids: Vec<SessionId> = self.list_by_sandbox(sandbox_id);
        for sid in &session_ids {
            if let Some(sender) = self.shutdown_senders.get(sid) {
                let _ = sender.send(Some(reason));
            }
        }
        session_ids
    }

    /// Returns the total count of active sessions.
    pub fn len(&self) -> usize {
        self.sessions.len()
    }

    /// Update the last_activity_at timestamp for a session.
    pub fn touch(&self, session_id: &SessionId) {
        if let Some(mut entry) = self.sessions.get_mut(session_id) {
            entry.touch();
        }
    }
}

impl Default for SessionTracker {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::thread;

    fn make_session(sandbox_id: &str, idle_timeout_secs: u64) -> TerminalSession {
        TerminalSession::new(
            sandbox_id.to_string(),
            "main".to_string(),
            "test-user".to_string(),
            "127.0.0.1:12345".to_string(),
            idle_timeout_secs,
        )
    }

    #[test]
    fn test_create_and_get() {
        let tracker = SessionTracker::new();
        let session = make_session("sb-abc", 1800);
        let id = session.session_id;
        tracker.create(session);

        assert!(tracker.get(&id).is_some());
        assert_eq!(tracker.get(&id).unwrap().sandbox_id, "sb-abc");
    }

    #[test]
    fn test_remove() {
        let tracker = SessionTracker::new();
        let session = make_session("sb-abc", 1800);
        let id = session.session_id;
        tracker.create(session);

        let removed = tracker.remove(&id);
        assert!(removed.is_some());
        assert!(tracker.get(&id).is_none());
    }

    #[test]
    fn test_list_by_sandbox() {
        let tracker = SessionTracker::new();
        let s1 = make_session("sb-1", 1800);
        let s2 = make_session("sb-1", 1800);
        let s3 = make_session("sb-2", 1800);

        let ids = vec![s1.session_id, s2.session_id, s3.session_id];
        tracker.create(s1);
        tracker.create(s2);
        tracker.create(s3);

        let sb1_sessions = tracker.list_by_sandbox("sb-1");
        assert_eq!(sb1_sessions.len(), 2);
        assert!(sb1_sessions.contains(&ids[0]));
        assert!(sb1_sessions.contains(&ids[1]));

        let sb2_sessions = tracker.list_by_sandbox("sb-2");
        assert_eq!(sb2_sessions.len(), 1);
    }

    #[test]
    fn test_remove_by_sandbox() {
        let tracker = SessionTracker::new();
        tracker.create(make_session("sb-1", 1800));
        tracker.create(make_session("sb-1", 1800));
        tracker.create(make_session("sb-2", 1800));

        let removed = tracker.remove_by_sandbox("sb-1");
        assert_eq!(removed.len(), 2);
        assert_eq!(tracker.len(), 1);
        assert!(tracker.list_by_sandbox("sb-1").is_empty());
    }

    #[test]
    fn test_idle_detection() {
        let tracker = SessionTracker::new();
        let session = TerminalSession::new(
            "sb-abc".to_string(),
            "main".to_string(),
            "user".to_string(),
            "127.0.0.1:12345".to_string(),
            0, // immediately idle
        );
        let id = session.session_id;
        tracker.create(session);

        // Small sleep to ensure elapsed time is > 0
        thread::sleep(std::time::Duration::from_millis(10));

        let idle = tracker.find_idle_sessions();
        assert!(idle.iter().any(|(sid, _)| *sid == id));
    }

    #[test]
    fn test_len() {
        let tracker = SessionTracker::new();
        assert_eq!(tracker.len(), 0);
        tracker.create(make_session("sb-1", 1800));
        tracker.create(make_session("sb-2", 1800));
        assert_eq!(tracker.len(), 2);
    }

    #[test]
    fn test_touch() {
        let tracker = SessionTracker::new();
        let session = TerminalSession::new(
            "sb-abc".to_string(),
            "main".to_string(),
            "user".to_string(),
            "127.0.0.1:12345".to_string(),
            3600, // 1 hour
        );
        let id = session.session_id;
        tracker.create(session);

        // Touch updates last_activity_at
        tracker.touch(&id);

        let entry = tracker.get(&id).unwrap();
        // Should not be idle with a 3600s timeout
        assert!(!entry.is_idle());
    }
}
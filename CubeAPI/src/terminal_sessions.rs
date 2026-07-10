// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//! Short-lived, single-use tickets for browser terminal upgrades.

use dashmap::DashMap;
use std::{sync::Arc, time::Instant};

#[derive(Debug, Clone)]
pub struct TerminalTicket {
    pub sandbox_id: String,
    pub container_id: String,
    pub host_ip: String,
    pub shell: String,
    pub username: Option<String>,
    pub expires_at: Instant,
}

#[derive(Clone, Default)]
pub struct TerminalTicketStore {
    tickets: Arc<DashMap<String, TerminalTicket>>,
}

impl TerminalTicketStore {
    pub fn insert(&self, ticket: String, session: TerminalTicket) {
        // Ticket records are intentionally in-memory and tiny. Prune expired
        // entries on writes so clients that never perform the WebSocket upgrade
        // cannot grow this map indefinitely.
        let now = Instant::now();
        self.tickets.retain(|_, existing| existing.expires_at > now);
        self.tickets.insert(ticket, session);
    }

    /// Consume a ticket exactly once. Expired tickets are rejected and removed.
    pub fn take(&self, ticket: &str) -> Option<TerminalTicket> {
        let (_, session) = self.tickets.remove(ticket)?;
        (session.expires_at > Instant::now()).then_some(session)
    }
}

#[cfg(test)]
mod tests {
    use super::{TerminalTicket, TerminalTicketStore};
    use std::time::{Duration, Instant};

    fn ticket(expires_at: Instant) -> TerminalTicket {
        TerminalTicket {
            sandbox_id: "sb-1".to_string(),
            container_id: "ctr-1".to_string(),
            host_ip: "127.0.0.1".to_string(),
            shell: "/bin/sh".to_string(),
            username: None,
            expires_at,
        }
    }

    #[test]
    fn ticket_can_only_be_consumed_once() {
        let store = TerminalTicketStore::default();
        store.insert(
            "one".to_string(),
            ticket(Instant::now() + Duration::from_secs(1)),
        );
        assert!(store.take("one").is_some());
        assert!(store.take("one").is_none());
    }

    #[test]
    fn expired_ticket_is_rejected() {
        let store = TerminalTicketStore::default();
        store.insert(
            "expired".to_string(),
            ticket(Instant::now() - Duration::from_secs(1)),
        );
        assert!(store.take("expired").is_none());
    }
}

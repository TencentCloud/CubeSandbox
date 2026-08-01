// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"strings"
	"testing"
	"time"
)

func TestTicketIssueRedeem(t *testing.T) {
	tm := NewTicketManager("test-secret", DefaultTicketTTL)

	ticket, err := tm.Issue("alice", "sbx-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	claims, err := tm.Redeem(ticket)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if claims.Username != "alice" || claims.SandboxID != "sbx-1" {
		t.Fatalf("claims = %+v, want alice/sbx-1", claims)
	}
}

func TestTicketSingleUse(t *testing.T) {
	tm := NewTicketManager("test-secret", DefaultTicketTTL)
	ticket, _ := tm.Issue("alice", "sbx-1")

	if _, err := tm.Redeem(ticket); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	_, err := tm.Redeem(ticket)
	if err == nil {
		t.Fatal("second Redeem succeeded, want rejection")
	}
	if !strings.Contains(err.Error(), "already used") {
		t.Fatalf("err = %v, want 'already used'", err)
	}
}

func TestTicketExpiry(t *testing.T) {
	tm := NewTicketManager("test-secret", time.Second)
	now := time.Now()
	tm.now = func() time.Time { return now }

	ticket, _ := tm.Issue("alice", "sbx-1")
	now = now.Add(2 * time.Second)

	if _, err := tm.Redeem(ticket); err == nil {
		t.Fatal("expired ticket was accepted")
	}
}

// A login access token must not open a terminal: the audience differs.
func TestTicketRejectsForeignAudience(t *testing.T) {
	tm := NewTicketManager("test-secret", DefaultTicketTTL)
	other := NewTicketManager("different-secret", DefaultTicketTTL)

	ticket, _ := other.Issue("mallory", "sbx-1")
	if _, err := tm.Redeem(ticket); err == nil {
		t.Fatal("ticket signed with a different secret was accepted")
	}
	if _, err := tm.Redeem("not-a-jwt"); err == nil {
		t.Fatal("garbage ticket was accepted")
	}
	if _, err := tm.Redeem(""); err == nil {
		t.Fatal("empty ticket was accepted")
	}
}

// Consumed tickets are dropped once they expire, so the map cannot grow
// without bound in a long-running process.
func TestTicketUsedMapIsPruned(t *testing.T) {
	tm := NewTicketManager("test-secret", time.Second)
	now := time.Now()
	tm.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		ticket, _ := tm.Issue("alice", "sbx-1")
		if _, err := tm.Redeem(ticket); err != nil {
			t.Fatalf("Redeem #%d: %v", i, err)
		}
	}
	if got := len(tm.used); got != 5 {
		t.Fatalf("used map has %d entries, want 5", got)
	}

	now = now.Add(10 * time.Second)
	fresh, _ := tm.Issue("alice", "sbx-1")
	if _, err := tm.Redeem(fresh); err != nil {
		t.Fatalf("Redeem fresh: %v", err)
	}
	if got := len(tm.used); got != 1 {
		t.Fatalf("used map has %d entries after pruning, want 1", got)
	}
}

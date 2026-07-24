// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"testing"
	"time"
)

func TestTerminalGrantFromProtocolsRequiresProtocolAndGrant(t *testing.T) {
	token, ok := terminalGrantFromProtocols([]string{terminalGrantPrefix + "abc", terminalProtocol})
	if !ok || token != "abc" {
		t.Fatalf("expected order-independent grant parse, got token=%q ok=%v", token, ok)
	}

	if token, ok := terminalGrantFromProtocols([]string{terminalGrantPrefix + "abc"}); ok || token != "abc" {
		t.Fatalf("expected grant without terminal protocol to be rejected, got token=%q ok=%v", token, ok)
	}

	if token, ok := terminalGrantFromProtocols([]string{terminalProtocol}); ok || token != "" {
		t.Fatalf("expected terminal protocol without grant to be rejected, got token=%q ok=%v", token, ok)
	}
}

func TestTerminalGrantStoreConsumesGrantOnce(t *testing.T) {
	store := newTerminalGrantStore()
	token, _, err := store.issue(terminalGrant{SandboxID: "sandbox-a", ContainerID: "container-a"})
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	grant, err := store.consume(token, "sandbox-a")
	if err != nil {
		t.Fatalf("consume grant: %v", err)
	}
	if grant.SandboxID != "sandbox-a" || grant.ContainerID != "container-a" {
		t.Fatalf("unexpected grant: %#v", grant)
	}

	if _, err := store.consume(token, "sandbox-a"); err == nil {
		t.Fatal("expected second consume to fail")
	}
}

func TestTerminalGrantStoreRejectsExpiredAndMismatchedGrant(t *testing.T) {
	store := newTerminalGrantStore()
	store.pending["expired"] = terminalGrant{SandboxID: "sandbox-a", ExpiresAt: time.Now().Add(-time.Second)}
	if _, err := store.consume("expired", "sandbox-a"); err == nil {
		t.Fatal("expected expired grant to fail")
	}

	token, _, err := store.issue(terminalGrant{SandboxID: "sandbox-a"})
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}
	if _, err := store.consume(token, "sandbox-b"); err == nil {
		t.Fatal("expected sandbox mismatch to fail")
	}
}

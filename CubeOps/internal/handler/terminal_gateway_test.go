// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"testing"
	"time"
)

func TestTerminalGrantIsSingleUseCookieBoundAndTargetBound(t *testing.T) {
	store := newTerminalGrantStore(terminalLimits{
		grantTTL:         time.Minute,
		pendingPerUser:   2,
		pendingGlobal:    4,
		activePerUser:    2,
		activePerSandbox: 2,
		activeGlobal:     4,
	})
	target := terminalTarget{sandboxID: "sandbox-1", containerID: "container-1", cols: 80, rows: 24}
	issued, err := store.issue("alice", target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.consume(issued.token, map[string]string{}); err != errTerminalBindingMismatch {
		t.Fatalf("consume without binding = %v, want %v", err, errTerminalBindingMismatch)
	}
	lease, err := store.consume(issued.token, map[string]string{issued.cookieName: issued.cookieValue})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	if lease.target != target || lease.principal != "alice" {
		t.Fatalf("lease = %#v, want target %#v for alice", lease, target)
	}
	if _, err := store.consume(issued.token, map[string]string{issued.cookieName: issued.cookieValue}); err != errTerminalUnknownGrant {
		t.Fatalf("second consume = %v, want %v", err, errTerminalUnknownGrant)
	}
}

func TestTerminalGrantLimitsAreReleasedWithLease(t *testing.T) {
	store := newTerminalGrantStore(terminalLimits{
		grantTTL:         time.Minute,
		pendingPerUser:   2,
		pendingGlobal:    2,
		activePerUser:    1,
		activePerSandbox: 1,
		activeGlobal:     1,
	})
	issue := func(container string) issuedTerminalGrant {
		grant, err := store.issue("alice", terminalTarget{sandboxID: "sandbox-1", containerID: container, cols: 80, rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}
	first := issue("container-1")
	lease, err := store.consume(first.token, map[string]string{first.cookieName: first.cookieValue})
	if err != nil {
		t.Fatal(err)
	}
	second := issue("container-2")
	if _, err := store.consume(second.token, map[string]string{second.cookieName: second.cookieValue}); err != errTerminalActiveLimit {
		t.Fatalf("consume while active = %v, want %v", err, errTerminalActiveLimit)
	}
	lease.release()
	third := issue("container-3")
	lease, err = store.consume(third.token, map[string]string{third.cookieName: third.cookieValue})
	if err != nil {
		t.Fatalf("consume after release: %v", err)
	}
	lease.release()
}

func TestTerminalOriginAndProtocols(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://console.example/terminal/sandboxes/sandbox-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "console.example"
	request.Header.Set("Origin", "https://console.example")
	request.Header.Set("Sec-WebSocket-Protocol", "cube-terminal.v1, cube-terminal.grant.secret")
	if !terminalOriginAllowed(request, "") {
		t.Fatal("same-host origin should be allowed")
	}
	if got := terminalGrantFromProtocols(request.Header); got != "secret" {
		t.Fatalf("grant = %q, want secret", got)
	}
	request.Header.Set("Origin", "https://attacker.example")
	if terminalOriginAllowed(request, "") {
		t.Fatal("cross-site origin should be rejected")
	}
	if !terminalOriginAllowed(request, "https://attacker.example,https://console.example") {
		t.Fatal("configured origin should be allowed")
	}
}

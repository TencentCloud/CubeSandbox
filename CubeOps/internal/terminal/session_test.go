// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package terminal

import (
	"testing"
	"time"
)

func newTestRegistry(t *testing.T, idleTimeout time.Duration) (*Registry, *fakeEnvd, func(time.Duration)) {
	t.Helper()
	f := newFakeEnvd(t)
	r := NewRegistry(NewClient("cube.app"), idleTimeout)
	now := time.Now()
	r.now = func() time.Time { return now }
	advance := func(d time.Duration) { now = now.Add(d) }
	return r, f, advance
}

func TestRegistrySweepKillsIdleSession(t *testing.T) {
	r, f, advance := newTestRegistry(t, 10*time.Minute)

	closed := make(chan string, 1)
	r.Add(&Session{ID: "s1", SandboxID: "sbx-1", PID: 42, CreatedAt: time.Now()},
		func(reason string) { closed <- reason })

	advance(5 * time.Minute)
	r.sweep()
	if r.CountForSandbox("sbx-1") != 1 {
		t.Fatal("session was reaped before the idle timeout")
	}

	advance(6 * time.Minute)
	r.sweep()
	if r.CountForSandbox("sbx-1") != 0 {
		t.Fatal("idle session was not reaped")
	}
	select {
	case reason := <-closed:
		if reason != "idle_timeout" {
			t.Fatalf("close reason = %q, want idle_timeout", reason)
		}
	default:
		t.Fatal("the WebSocket was not closed")
	}
	if _, _, _, signals := f.snapshot(); signals != 1 {
		t.Fatalf("SIGKILL count = %d, want 1", signals)
	}
}

// Activity resets the idle clock: a terminal in use is never reaped.
func TestRegistryTouchDefersSweep(t *testing.T) {
	r, _, advance := newTestRegistry(t, 10*time.Minute)
	r.Add(&Session{ID: "s1", SandboxID: "sbx-1", PID: 42, CreatedAt: time.Now()}, func(string) {})

	for i := 0; i < 3; i++ {
		advance(9 * time.Minute)
		r.Touch("s1")
		r.sweep()
		if r.CountForSandbox("sbx-1") != 1 {
			t.Fatalf("active session reaped on iteration %d", i)
		}
	}

	advance(11 * time.Minute)
	r.sweep()
	if r.CountForSandbox("sbx-1") != 0 {
		t.Fatal("session was not reaped after activity stopped")
	}
}

// A detached session (socket dropped, PTY alive) still idles out, so a
// browser that never comes back cannot leak shells inside the sandbox.
func TestRegistrySweepReapsDetachedSession(t *testing.T) {
	r, f, advance := newTestRegistry(t, 10*time.Minute)
	r.Add(&Session{ID: "s1", SandboxID: "sbx-1", PID: 42, CreatedAt: time.Now()}, func(string) {})
	r.Detach("s1")

	advance(11 * time.Minute)
	r.sweep()

	if r.CountForSandbox("sbx-1") != 0 {
		t.Fatal("detached session was not reaped")
	}
	if _, _, _, signals := f.snapshot(); signals != 1 {
		t.Fatalf("SIGKILL count = %d, want 1", signals)
	}
}

func TestRegistryReattachRequiresDetachedMatch(t *testing.T) {
	r, _, _ := newTestRegistry(t, 10*time.Minute)
	r.Add(&Session{ID: "s1", SandboxID: "sbx-1", PID: 42, CreatedAt: time.Now()}, func(string) {})

	if got := r.Reattach("sbx-1", 42, func(string) {}); got != nil {
		t.Fatal("reattached to a session that is still attached")
	}
	r.Detach("s1")
	if got := r.Reattach("sbx-1", 99, func(string) {}); got != nil {
		t.Fatal("reattached with the wrong PID")
	}
	if got := r.Reattach("sbx-2", 42, func(string) {}); got != nil {
		t.Fatal("reattached across sandboxes")
	}
	got := r.Reattach("sbx-1", 42, func(string) {})
	if got == nil || got.ID != "s1" {
		t.Fatalf("Reattach = %+v, want session s1", got)
	}
	// A claimed session cannot be stolen by a second connection.
	if again := r.Reattach("sbx-1", 42, func(string) {}); again != nil {
		t.Fatal("a second connection stole an attached session")
	}
}

// Sessions are per-sandbox; counting must not bleed across sandboxes.
func TestRegistryCountIsPerSandbox(t *testing.T) {
	r, _, _ := newTestRegistry(t, 10*time.Minute)
	r.Add(&Session{ID: "s1", SandboxID: "sbx-1", PID: 1, CreatedAt: time.Now()}, func(string) {})
	r.Add(&Session{ID: "s2", SandboxID: "sbx-1", PID: 2, CreatedAt: time.Now()}, func(string) {})
	r.Add(&Session{ID: "s3", SandboxID: "sbx-2", PID: 3, CreatedAt: time.Now()}, func(string) {})

	if got := r.CountForSandbox("sbx-1"); got != 2 {
		t.Fatalf("sbx-1 count = %d, want 2", got)
	}
	if got := r.CountForSandbox("sbx-2"); got != 1 {
		t.Fatalf("sbx-2 count = %d, want 1", got)
	}
	r.Remove("s1")
	if got := r.CountForSandbox("sbx-1"); got != 1 {
		t.Fatalf("sbx-1 count after Remove = %d, want 1", got)
	}
}

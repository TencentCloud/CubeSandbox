// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import "testing"

func TestSubprotocolToken(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   string
	}{
		{"joined", []string{"cube-terminal, tok123"}, "tok123"},
		{"separate headers", []string{"cube-terminal", "tok123"}, "tok123"},
		{"marker only", []string{"cube-terminal"}, ""},
		{"empty", nil, ""},
		{"token first", []string{"tok123, cube-terminal"}, "tok123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := subprotocolToken(tc.values); got != tc.want {
				t.Fatalf("subprotocolToken(%v) = %q, want %q", tc.values, got, tc.want)
			}
		})
	}
}

func TestTerminalOpenMatchesPath(t *testing.T) {
	cases := []struct {
		name  string
		frame terminalClientFrame
		path  string
		want  bool
	}{
		{"matching id", terminalClientFrame{SandboxID: "sb-1"}, "sb-1", true},
		{"mismatched id", terminalClientFrame{SandboxID: "sb-2"}, "sb-1", false},
		// An omitted sandboxId previously skipped the check entirely; the frame
		// must now name the sandbox for the session to open.
		{"omitted id", terminalClientFrame{}, "sb-1", false},
		{"empty path", terminalClientFrame{SandboxID: "sb-1"}, "", false},
		{"both empty", terminalClientFrame{}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalOpenMatchesPath(tc.frame, tc.path); got != tc.want {
				t.Fatalf("terminalOpenMatchesPath(%+v, %q) = %v, want %v",
					tc.frame, tc.path, got, tc.want)
			}
		})
	}
}

func TestTerminalPingIntervalIsBelowIdleTimeout(t *testing.T) {
	// A ping interval at or above the idle timeout would never detect a dead
	// peer before the session expired on its own, defeating the purpose.
	if terminalPingInterval >= terminalIdleTimeout {
		t.Fatalf("terminalPingInterval (%v) must be below terminalIdleTimeout (%v)",
			terminalPingInterval, terminalIdleTimeout)
	}
	if terminalPingInterval <= terminalWriteTimeout {
		t.Fatalf("terminalPingInterval (%v) should exceed terminalWriteTimeout (%v) "+
			"so a slow write cannot overlap the next ping", terminalPingInterval, terminalWriteTimeout)
	}
}

func TestClampDim(t *testing.T) {
	cases := []struct{ in, max, want int }{
		{0, 512, 0},
		{-5, 512, 0},
		{80, 512, 80},
		{1000, 512, 512},
	}
	for _, tc := range cases {
		if got := clampDim(tc.in, tc.max); got != tc.want {
			t.Fatalf("clampDim(%d,%d) = %d, want %d", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestTerminalSessionLimiter(t *testing.T) {
	l := newTerminalSessionLimiter(2)
	if !l.acquire("s1") || !l.acquire("s1") {
		t.Fatalf("first two acquires should succeed")
	}
	if l.acquire("s1") {
		t.Fatalf("third acquire should be rejected")
	}
	// A different sandbox has its own budget.
	if !l.acquire("s2") {
		t.Fatalf("acquire for a different sandbox should succeed")
	}
	l.release("s1")
	if !l.acquire("s1") {
		t.Fatalf("acquire should succeed after release")
	}
}

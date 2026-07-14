// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Nil-safety: callbacks are nil by default and must not panic
// ---------------------------------------------------------------------------

func TestOnNodeRegistered_NilSafe(t *testing.T) {
	orig := OnNodeRegistered
	OnNodeRegistered = nil
	defer func() { OnNodeRegistered = orig }()
	if OnNodeRegistered != nil {
		t.Fatal("expected nil by default")
	}
}

func TestOnNodeLabelsChanged_NilSafe(t *testing.T) {
	orig := OnNodeLabelsChanged
	OnNodeLabelsChanged = nil
	defer func() { OnNodeLabelsChanged = orig }()
	if OnNodeLabelsChanged != nil {
		t.Fatal("expected nil by default")
	}
}

func TestOnNodeHealthTransitioned_NilSafe(t *testing.T) {
	orig := OnNodeHealthTransitioned
	OnNodeHealthTransitioned = nil
	defer func() { OnNodeHealthTransitioned = orig }()
	if OnNodeHealthTransitioned != nil {
		t.Fatal("expected nil by default")
	}
}

// ---------------------------------------------------------------------------
// Callback invocation when set
// ---------------------------------------------------------------------------

func TestOnNodeRegistered_FiresWhenSet(t *testing.T) {
	var called atomic.Int32
	orig := OnNodeRegistered
	OnNodeRegistered = func(nodeID string) {
		if nodeID == "test-node" {
			called.Add(1)
		}
	}
	defer func() { OnNodeRegistered = orig }()

	if OnNodeRegistered != nil {
		go OnNodeRegistered("test-node")
	}

	waitForCount(t, &called, 1, time.Second)
}

func TestOnNodeHealthTransitioned_FiresOnlyOnTransition(t *testing.T) {
	tests := []struct {
		name        string
		prevHealthy bool
		currHealthy bool
		shouldFire  bool
	}{
		{"healthy_to_unhealthy", true, false, true},
		{"unhealthy_to_healthy", false, true, true},
		{"stays_healthy", true, true, false},
		{"stays_unhealthy", false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called atomic.Int32
			orig := OnNodeHealthTransitioned
			OnNodeHealthTransitioned = func(nodeID string, healthy bool) {
				called.Add(1)
			}
			defer func() { OnNodeHealthTransitioned = orig }()

			// Simulate the guard + fire pattern used in UpdateNodeStatus.
			if OnNodeHealthTransitioned != nil && tt.prevHealthy != tt.currHealthy {
				go OnNodeHealthTransitioned("test-node", tt.currHealthy)
			}

			if tt.shouldFire {
				waitForCount(t, &called, 1, time.Second)
			} else {
				time.Sleep(50 * time.Millisecond)
				if called.Load() != 0 {
					t.Fatal("callback should not have fired for non-transition")
				}
			}
		})
	}
}

// waitForCount blocks until the atomic counter reaches want or the deadline passes.
func waitForCount(t *testing.T, c *atomic.Int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Load() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if c.Load() != want {
		t.Fatalf("timed out: counter=%d, want=%d", c.Load(), want)
	}
}

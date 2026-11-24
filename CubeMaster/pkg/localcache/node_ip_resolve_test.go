// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func setupIsolatedNodeCache(t *testing.T) {
	t.Helper()
	t.Cleanup(ResetNodeCacheForTest())
}

func TestResolveNodeIPFollowsNodeIPChange(t *testing.T) {
	setupIsolatedNodeCache(t)

	UpsertNode(&node.Node{
		InsID:   "k8s-node-1",
		IP:      "10.1.0.10",
		Healthy: true,
	})

	if got := ResolveNodeIP("k8s-node-1", "10.1.0.10"); got != "10.1.0.10" {
		t.Fatalf("initial ResolveNodeIP = %q, want 10.1.0.10", got)
	}
	if got := ResolveNodeIP("", "10.1.0.10"); got != "10.1.0.10" {
		t.Fatalf("empty nodeID should fall back, got %q", got)
	}
	if got := ResolveNodeIP("unknown-node", "10.1.0.10"); got != "10.1.0.10" {
		t.Fatalf("unknown nodeID should fall back, got %q", got)
	}

	// Simulate BigPod recreate with a new Pod IP under the same NodeID.
	UpsertNode(&node.Node{
		InsID:   "k8s-node-1",
		IP:      "10.1.0.20",
		Healthy: true,
	})

	if got := ResolveNodeIP("k8s-node-1", "10.1.0.10"); got != "10.1.0.20" {
		t.Fatalf("post-update ResolveNodeIP = %q, want 10.1.0.20", got)
	}
}

func TestSandboxCacheFollowsNodeIPChangeEvenWhenOldIPIsReused(t *testing.T) {
	setupIsolatedNodeCache(t)

	UpsertNode(&node.Node{
		InsID:   "k8s-node-a",
		IP:      "10.2.0.5",
		Healthy: true,
	})

	// SetSandboxCache auto-fills HostID from current HostIP when omitted.
	SetSandboxCache("sb-1", &SandboxCache{
		SandboxID: "sb-1",
		HostIP:    "10.2.0.5",
	})

	// Node A gets a new Pod IP, and Node B later receives Node A's old Pod IP.
	UpsertNode(&node.Node{
		InsID:   "k8s-node-a",
		IP:      "10.2.0.9",
		Healthy: true,
	})
	UpsertNode(&node.Node{
		InsID:   "k8s-node-b",
		IP:      "10.2.0.5",
		Healthy: true,
	})

	got := GetSandboxCache("sb-1")
	if got == nil {
		t.Fatal("expected cached sandbox entry")
	}
	if got.HostID != "k8s-node-a" {
		t.Fatalf("HostID = %q, want k8s-node-a", got.HostID)
	}
	if got.HostIP != "10.2.0.9" {
		t.Fatalf("HostIP = %q, want 10.2.0.9 (must follow node-a, not collide with node-b)", got.HostIP)
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package task

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
)

func TestResolveTaskCallEpFollowsNodeIPChange(t *testing.T) {
	t.Cleanup(localcache.ResetNodeCacheForTest())

	origAddrFn := cubeletAddrFn
	cubeletAddrFn = func(ip string) string { return ip + ":50051" }
	t.Cleanup(func() { cubeletAddrFn = origAddrFn })

	localcache.SetNodeForTest(&node.Node{
		InsID:   "k8s-node-task-1",
		IP:      "10.9.0.11",
		Healthy: true,
	})

	queued := &Task{
		NodeID: "k8s-node-task-1",
		CallEp: "10.9.0.11:50051",
	}

	// Simulate BigPod IP change while the task is queued/retrying.
	localcache.SetNodeForTest(&node.Node{
		InsID:   "k8s-node-task-1",
		IP:      "10.9.0.22",
		Healthy: true,
	})

	ep, hostIP, ok := resolveTaskCallEp(queued)
	if !ok {
		t.Fatalf("resolveTaskCallEp returned ok=false, want true")
	}
	if hostIP != "10.9.0.22" {
		t.Fatalf("hostIP = %q, want 10.9.0.22", hostIP)
	}
	if ep != "10.9.0.22:50051" {
		t.Fatalf("callEp = %q, want 10.9.0.22:50051", ep)
	}

	// Task with NodeID absent from localcache must fail closed instead of
	// falling back to a potentially recycled legacy IP.
	missing := &Task{
		NodeID: "k8s-node-deleted",
		CallEp: "10.9.0.44:50051",
	}
	if ep, hostIP, ok := resolveTaskCallEp(missing); ok || ep != "" || hostIP != "" {
		t.Fatalf("missing node resolved to (%q, %q, %v), want (\"\", \"\", false)", ep, hostIP, ok)
	}

	// Legacy task without NodeID keeps its original endpoint.
	legacy := &Task{CallEp: "10.9.0.33:50051"}
	legacyEp, legacyIP, legacyOK := resolveTaskCallEp(legacy)
	if !legacyOK || legacyIP != "10.9.0.33" || legacyEp != legacy.CallEp {
		t.Fatalf("legacy task resolved to (%q, %q, %v), want (%q, 10.9.0.33, true)", legacyEp, legacyIP, legacyOK, legacy.CallEp)
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

// TestRemoveNodePreInitRemoves: before Init the tooling path (schedsim) must
// actually withdraw the node from the cache and the sorted index.
func TestRemoveNodePreInitRemoves(t *testing.T) {
	if inited.Load() {
		t.Fatal("test requires the process to be pre-Init")
	}
	n := &node.Node{InsID: "ins-removenode-preinit", OssClusterLabel: "test-cluster"}
	UpsertNode(n)
	if _, ok := l.cache.Get(n.ID()); !ok {
		t.Fatalf("node %q was not injected", n.ID())
	}
	if err := RemoveNode(context.Background(), &node.Node{InsID: n.InsID, OssClusterLabel: n.OssClusterLabel}); err != nil {
		t.Fatalf("RemoveNode pre-Init returned an error: %v", err)
	}
	if _, ok := l.cache.Get(n.ID()); ok {
		t.Fatalf("node %q still cached after RemoveNode", n.ID())
	}
	for _, list := range l.sortedNodesByClusters {
		for _, sn := range list {
			if sn != nil && sn.ID() == n.ID() {
				t.Fatalf("node %q still in the sorted index after RemoveNode", n.ID())
			}
		}
	}
}

// TestRemoveNodeRefusedAfterInit: once the Init latch is set, RemoveNode must
// refuse with an error and leave the node untouched — the refusal is what
// lets a caller (e.g. sim cleanup) detect that the node was NOT removed when
// the log line is suppressed by a FATAL log level.
func TestRemoveNodeRefusedAfterInit(t *testing.T) {
	inited.Store(true)
	defer inited.Store(false)
	n := &node.Node{InsID: "ins-removenode-inited", OssClusterLabel: "test-cluster"}
	UpsertNode(n)
	// Clean up directly, bypassing the guard this test flips.
	defer l.delNodeCache(context.Background(), n)

	err := RemoveNode(context.Background(), &node.Node{InsID: n.InsID, OssClusterLabel: n.OssClusterLabel})
	if err == nil {
		t.Fatal("RemoveNode with the Init latch set: want a refusal error, got nil")
	}
	if !strings.Contains(err.Error(), "Init") {
		t.Fatalf("refusal error should name the Init latch, got: %v", err)
	}
	if _, ok := l.cache.Get(n.ID()); !ok {
		t.Fatalf("refused RemoveNode still removed node %q", n.ID())
	}
}

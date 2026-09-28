// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func testNode(id, ip string) *node.Node { return &node.Node{InsID: id, IP: ip} }

// The selection must skip nodes with a READY clean replica (matched by node
// id OR ip), and re-target nodes whose replica is failed or still
// cleanup_required (half-cleaned by an interrupted delete).
func TestNodesWithoutReadyReplica(t *testing.T) {
	replicas := []models.TemplateReplica{
		{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		{NodeID: "node-c", NodeIP: "10.0.0.3", Status: ReplicaStatusReady, CleanupRequired: true},
	}
	targets := []*node.Node{
		testNode("node-a", "10.0.0.1"),
		testNode("node-b", "10.0.0.2"),
		testNode("node-c", "10.0.0.3"),
		testNode("node-new", "10.0.0.4"),
		nil, // tolerated
	}
	missing := nodesWithoutReadyReplica(replicas, targets)
	got := make([]string, 0, len(missing))
	for _, n := range missing {
		got = append(got, n.ID())
	}
	want := []string{"node-b", "node-c", "node-new"}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing = %v, want %v", got, want)
		}
	}
}

// A replica row keyed by IP only must still count as covering the node.
func TestNodesWithoutReadyReplicaIPOnlyRow(t *testing.T) {
	replicas := []models.TemplateReplica{{NodeIP: "10.0.0.1", Status: ReplicaStatusReady}}
	targets := []*node.Node{testNode("node-a", "10.0.0.1")}
	if missing := nodesWithoutReadyReplica(replicas, targets); len(missing) != 0 {
		t.Fatalf("missing = %v, want none (IP-only replica covers the node)", missing)
	}
}

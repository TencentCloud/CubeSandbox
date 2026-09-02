// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package selctx

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

func TestSorted(t *testing.T) {
	nodes := node.NodeList{}
	testNum := 10
	for i := 1; i <= testNum; i++ {
		n := &node.Node{
			Index: i,
			InsID: fmt.Sprintf("%d", i),
		}
		nodes.Append(n)
	}
	if testNum != nodes.Len() {
		t.Fatalf("testNum != nodes.Len(), testNum: %d, nodes.Len(): %d", testNum, nodes.Len())
	}
	nodes.AllSortByIndex()
	slctx := New("")
	slctx.SetNodes(nodes)

	if slctx.Nodes().Len() != testNum {
		t.Fatalf("slctx.Nodes().Len() != testNum, slctx.Nodes().Len(): %d, testNum: %d", slctx.Nodes().Len(), testNum)
	}

	tmplist := slctx.LeastNodes(-1)
	if tmplist.Len() != testNum {
		t.Fatalf("tmplist.Len() != testNum, tmplist.Len(): %d, testNum: %d", tmplist.Len(), testNum)
	}

	tmplist = slctx.LeastNodes(0)
	if tmplist.Len() != 0 {
		t.Fatalf("tmplist.Len() != 0, tmplist.Len(): %d, testNum: %d", tmplist.Len(), 0)
	}
	tmplist = slctx.LeastNodes(1)
	if tmplist.Len() != 1 {
		t.Fatalf("tmplist.Len() != 1, tmplist.Len(): %d, testNum: %d", tmplist.Len(), 1)
	}
	tmplist = slctx.LeastNodes(4)
	if tmplist.Len() != 4 {
		t.Fatalf("tmplist.Len() != 4, tmplist.Len(): %d, testNum: %d", tmplist.Len(), 4)
	}
	tmplist = slctx.LeastNodes(10)
	if tmplist.Len() != 10 {
		t.Fatalf("tmplist.Len() != 10, tmplist.Len(): %d, testNum: %d", tmplist.Len(), 10)
	}

	tmpNode := slctx.LeastRandomSelect(1)
	if tmpNode == nil {
		t.Fatalf("tmpNode == nil")
	}
	assert.Equal(t, 1, tmpNode.Index)
}

func TestFreezeSnapshotIsIdempotent(t *testing.T) {
	slctx := New("")
	slctx.SetNodes(node.NodeList{{InsID: "n1"}, {InsID: "n2"}})
	slctx.FreezeSnapshot()
	version := slctx.SnapshotVersion
	if version == "" {
		t.Fatal("SnapshotVersion is empty after FreezeSnapshot")
	}
	if slctx.SnapshotNodes().Len() != 2 {
		t.Fatalf("SnapshotNodes().Len() = %d, want 2", slctx.SnapshotNodes().Len())
	}
	// Narrow the candidate set, then re-freeze: both the version and the
	// snapshot must stay exactly as the first freeze left them.
	slctx.SetNodes(slctx.Nodes()[1:])
	slctx.FreezeSnapshot()
	if slctx.SnapshotVersion != version {
		t.Fatalf("SnapshotVersion changed on re-freeze: %q -> %q", version, slctx.SnapshotVersion)
	}
	if slctx.SnapshotNodes().Len() != 2 {
		t.Fatalf("SnapshotNodes().Len() = %d after re-freeze, want 2", slctx.SnapshotNodes().Len())
	}
	if slctx.Nodes().Len() != 1 {
		t.Fatalf("Nodes().Len() = %d, want narrowed result of 1", slctx.Nodes().Len())
	}
}

func TestFreezeSnapshotVersionsAreUnique(t *testing.T) {
	first, second := New(""), New("")
	first.SetNodes(node.NodeList{{InsID: "n1"}})
	second.SetNodes(node.NodeList{{InsID: "n1"}})
	first.FreezeSnapshot()
	second.FreezeSnapshot()
	if first.SnapshotVersion == second.SnapshotVersion {
		t.Fatalf("SnapshotVersion %q is not unique per request", first.SnapshotVersion)
	}
}

func TestFreezeSnapshotDefersUntilCandidatesAttached(t *testing.T) {
	slctx := New("")
	// No candidate pool yet: the freeze must be deferred, leaving the version
	// empty so external plugins fail closed instead of syncing an empty pool.
	slctx.FreezeSnapshot()
	if slctx.SnapshotVersion != "" {
		t.Fatalf("SnapshotVersion = %q, want empty before candidates attach", slctx.SnapshotVersion)
	}
	slctx.SetNodes(node.NodeList{{InsID: "n1"}})
	slctx.FreezeSnapshot()
	if slctx.SnapshotVersion == "" {
		t.Fatal("SnapshotVersion is empty after candidates attached")
	}
	if slctx.SnapshotNodes().Len() != 1 {
		t.Fatalf("SnapshotNodes().Len() = %d, want 1", slctx.SnapshotNodes().Len())
	}
}

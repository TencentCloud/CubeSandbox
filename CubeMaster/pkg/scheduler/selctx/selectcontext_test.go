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

// TestLeastRandomSelectClearsStaleItems locks in the admission-failure
// reselect behavior: the reused SelectorCtx must not return a node that was
// marked last-bad after the first pick, even though the selector accumulated
// it during that pick.
func TestLeastRandomSelectClearsStaleItems(t *testing.T) {
	bad := &node.Node{InsID: "bad"}
	good := &node.Node{InsID: "good"}

	slctx := New("random")
	slctx.SetNodes(node.NodeList{bad})
	if got := slctx.LeastRandomSelect(1); got == nil || got.ID() != "bad" {
		t.Fatalf("first pick = %v, want the only candidate (bad)", got)
	}

	// Admission failure on the selected node: mark it bad and reselect
	// from the remaining candidates with the same SelectorCtx.
	slctx.AddLastBadNode(bad)
	slctx.SetNodes(node.NodeList{good})
	for i := 0; i < 100; i++ {
		if got := slctx.LeastRandomSelect(1); got == nil || got.ID() != "good" {
			t.Fatalf("reselect iteration %d returned stale node: %v", i, got)
		}
	}
}

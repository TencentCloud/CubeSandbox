// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package framework

import (
	"sync"
	"testing"
)

func TestImageStateSummary_ConcurrentNodesAccess(t *testing.T) {

	iss := NewImageStateSummary(0, "")

	const (
		numGoroutines = 10
		numOperations = 100
		nodePrefix    = "node-"
	)

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			for j := 0; j < numOperations; j++ {
				nodeName := nodePrefix + string(rune(id)) + "-" + string(rune(j))

				iss.AddNode(nodeName)

				if !iss.HasNode(nodeName) {
					t.Errorf("节点 %s 应该存在但未找到", nodeName)
				}

				iss.RemoveNode(nodeName)

				if iss.HasNode(nodeName) {
					t.Errorf("节点 %s 应该被删除但仍然存在", nodeName)
				}
			}
		}(i)
	}

	wg.Wait()

	if iss.GetNumNodes() != 0 {
		t.Errorf("最终Nodes集合应该为空，但实际有 %d 个元素", iss.GetNumNodes())
	}
}

func TestImageStateSummary_ConcurrentReadWrite(t *testing.T) {
	iss := NewImageStateSummary(0, "")

	const (
		numReaders = 5
		numWriters = 5
		numNodes   = 50
	)

	var wg sync.WaitGroup

	for i := 0; i < numNodes; i++ {
		nodeName := "preload-node-" + string(rune(i))
		iss.AddNode(nodeName)
	}

	wg.Add(numWriters)
	for i := 0; i < numWriters; i++ {
		go func(writerID int) {
			defer wg.Done()

			for j := 0; j < 20; j++ {
				nodeName := "writer-" + string(rune(writerID)) + "-node-" + string(rune(j))

				iss.AddNode(nodeName)

				if j < numNodes {
					deleteNode := "preload-node-" + string(rune(j))
					iss.RemoveNode(deleteNode)
				}
			}
		}(i)
	}

	wg.Add(numReaders)
	for i := 0; i < numReaders; i++ {
		go func(readerID int) {
			defer wg.Done()

			for j := 0; j < 30; j++ {

				length := iss.GetNumNodes()
				if length < 0 {
					t.Errorf("Nodes长度不能为负数: %d", length)
				}

				if j < numNodes {
					nodeName := "preload-node-" + string(rune(j))
					_ = iss.HasNode(nodeName)
				}
			}
		}(i)
	}

	wg.Wait()

	t.Logf("并发读写测试完成，最终Nodes数量: %d", iss.GetNumNodes())
}

func TestImageStateSummary_SnapshotConcurrency(t *testing.T) {
	iss := NewImageStateSummary(1024, "")

	for i := 0; i < 10; i++ {
		nodeName := "initial-node-" + string(rune(i))
		iss.AddNode(nodeName)
	}

	const numSnapshots = 100
	var wg sync.WaitGroup
	wg.Add(numSnapshots)

	snapshots := make([]*ImageStateSummary, numSnapshots)
	for i := 0; i < numSnapshots; i++ {
		go func(index int) {
			defer wg.Done()
			snapshots[index] = iss.Snapshot()
		}(i)
	}

	wg.Wait()

	expectedNumNodes := iss.GetNumNodes()
	for i, snapshot := range snapshots {
		if snapshot.NumNodes != expectedNumNodes {
			t.Errorf("快照 %d 的NumNodes不正确: 期望 %d, 实际 %d", i, expectedNumNodes, snapshot.NumNodes)
		}
		if snapshot.Size != iss.Size {
			t.Errorf("快照 %d 的Size不正确: 期望 %d, 实际 %d", i, iss.Size, snapshot.Size)
		}
	}
}

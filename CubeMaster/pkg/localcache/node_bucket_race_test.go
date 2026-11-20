// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

// bench-free helpers for the race regression: build a healthy node with a
// stable id in the default instance type.
func raceNode(i int) *node.Node {
	n := &node.Node{
		InsID:        fmt.Sprintf("race-node-%d", i),
		IP:           fmt.Sprintf("10.9.%d.%d", i/250, i%250),
		Healthy:      true,
		ClusterLabel: "race",
		MaxMvmLimit:  2000,
		QuotaCpu:     2880000,
		QuotaMem:     983040,
	}
	return n
}

// raceResetBuckets empties the sorted buckets so the test starts from a clean
// default-type bucket.
func raceResetBuckets() {
	l.lockSortedNodes.Lock()
	l.sortedNodesByClusters[constants.DefaultInstanceTypeName] = node.NodeList{}
	l.lockSortedNodes.Unlock()
}

// TestSortedBucketSnapshotStableWhileWriterUpdates is the regression test for
// the shared-bucket race: a reader retains and iterates a NodeList snapshot
// while writers append, remove and re-sort the same instance-type bucket.
// The writer must never mutate the snapshot the reader holds; without the
// copy-on-write bucket update this trips `go test -race`.
func TestSortedBucketSnapshotStableWhileWriterUpdates(t *testing.T) {
	raceResetBuckets()
	defer raceResetBuckets()

	const baseNodes = 50
	for i := 0; i < baseNodes; i++ {
		l.appendSortedNodes(raceNode(i))
	}

	// Reader keeps its snapshot across the whole writer storm.
	snapshot := func() node.NodeList {
		l.lockSortedNodes.RLock()
		defer l.lockSortedNodes.RUnlock()
		clusterNodes := l.sortedNodesByClusters[constants.DefaultInstanceTypeName]
		return node.NodeList(clusterNodes)
	}()

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for iter := 0; iter < 200; iter++ {
			// The snapshot must stay sorted by index and free of duplicates
			// for the whole iteration, exactly what a torn in-place reorder
			// would break.
			prev := -1
			seen := map[string]bool{}
			for _, nd := range snapshot {
				require.False(t, seen[nd.ID()], "duplicate node %s in snapshot", nd.ID())
				seen[nd.ID()] = true
				require.GreaterOrEqual(t, nd.Index, prev, "snapshot order changed under the reader")
				prev = nd.Index
			}
		}
	}()

	const writers = 4
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id := baseNodes + writer*100 + i
				l.appendSortedNodes(raceNode(id))
				l.updateSortedNodes(raceNode(id))
				if i%10 == 0 {
					l.delSortedNodes(raceNode(id))
				}
			}
		}(w)
	}
	wg.Wait()
	<-readerDone
}

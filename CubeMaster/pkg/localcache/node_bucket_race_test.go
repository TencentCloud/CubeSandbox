// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package localcache

import (
	"fmt"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

// raceNode builds a healthy node in the default instance type with a distinct
// index. Index must be distinct: AllSortByIndex compares Index, so equal
// values make sort.Slice perform zero swaps and the race never happens.
func raceNode(id string, index int) *node.Node {
	return &node.Node{
		InsID:        id,
		IP:           "10.9.0.1",
		Healthy:      true,
		ClusterLabel: "race",
		Index:        index,
		MaxMvmLimit:  2000,
		QuotaCpu:     2880000,
		QuotaMem:     983040,
	}
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

	// Seed with even indexes and leave the odd ones free. Writers then insert
	// between existing nodes, so an in-place re-sort has to move elements that
	// the reader is iterating over. Spare capacity keeps those appends in the
	// same backing array as the snapshot, which is what makes the race
	// observable on the pre-fix code.
	const baseNodes = 50
	seeded := make(node.NodeList, 0, baseNodes*8)
	for i := 0; i < baseNodes; i++ {
		seeded = append(seeded, raceNode(fmt.Sprintf("race-node-seed-%d", i), i*2))
	}
	l.lockSortedNodes.Lock()
	l.sortedNodesByClusters[constants.DefaultInstanceTypeName] = seeded
	l.lockSortedNodes.Unlock()

	// Reader keeps its snapshot across the whole writer storm.
	snapshot := seeded

	writersDone := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-writersDone:
				return
			default:
			}
			// The snapshot must stay sorted by index and free of duplicates
			// for the whole iteration, exactly what a torn in-place reorder
			// would break.
			prev := -1
			seen := make(map[string]struct{}, len(snapshot))
			for _, nd := range snapshot {
				if _, dup := seen[nd.ID()]; dup {
					t.Errorf("duplicate node %s in snapshot", nd.ID())
					return
				}
				seen[nd.ID()] = struct{}{}
				if nd.Index < prev {
					t.Errorf("snapshot order changed under the reader: index %d after %d", nd.Index, prev)
					return
				}
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
				// Odd index, always inside the seeded range, so every insert
				// shifts the elements the reader holds.
				n := raceNode(fmt.Sprintf("race-node-w%d-%d", writer, i), i%baseNodes*2+1)
				l.appendSortedNodes(n)
				l.updateSortedNodes(n)
				if i%10 == 0 {
					l.delSortedNodes(n)
				}
			}
		}(w)
	}
	wg.Wait()
	close(writersDone)
	<-readerDone
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"sort"
	"testing"
)

func resetSnapshotStorageStateCacheForTest(t *testing.T) {
	t.Helper()
	snapshotStorageStateCache.Lock()
	snapshotStorageStateCache.byNode = make(map[string]SnapshotStorageState)
	snapshotStorageStateCache.Unlock()
}

func TestListSnapshotStorageStatesDeduplicatesByNodeIDAndIP(t *testing.T) {
	resetSnapshotStorageStateCacheForTest(t)

	SetSnapshotStorageState(SnapshotStorageState{
		NodeID:        "node-a",
		NodeIP:        "10.0.0.1",
		Mode:          "healthy",
		LastUpdatedAt: 100,
	})
	SetSnapshotStorageState(SnapshotStorageState{
		NodeID:        "node-b",
		NodeIP:        "10.0.0.2",
		Mode:          "unknown",
		LastUpdatedAt: 200,
	})

	got := ListSnapshotStorageStates()
	if len(got) != 2 {
		t.Fatalf("expected 2 unique entries, got %d: %#v", len(got), got)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].NodeID < got[j].NodeID })

	if got[0].NodeID != "node-a" || got[0].NodeIP != "10.0.0.1" || got[0].Mode != "healthy" {
		t.Fatalf("unexpected entry[0]: %#v", got[0])
	}
	if got[1].NodeID != "node-b" || got[1].NodeIP != "10.0.0.2" || got[1].Mode != "unknown" {
		t.Fatalf("unexpected entry[1]: %#v", got[1])
	}
}

func TestListSnapshotStorageStatesEmpty(t *testing.T) {
	resetSnapshotStorageStateCacheForTest(t)
	if got := ListSnapshotStorageStates(); got != nil {
		t.Fatalf("expected nil for empty cache, got %#v", got)
	}
}

func TestListSnapshotStorageStatesIgnoresPartialOverwrite(t *testing.T) {
	resetSnapshotStorageStateCacheForTest(t)

	// Simulate SetSnapshotStorageState writing two byNode entries indexed by
	// NodeID and NodeIP. Under the hood they refer to the same (NodeID, NodeIP)
	// entity, so listing must deduplicate it.
	SetSnapshotStorageState(SnapshotStorageState{
		NodeID:        "node-x",
		NodeIP:        "10.0.0.9",
		Mode:          "warn",
		LastUpdatedAt: 300,
	})

	got := ListSnapshotStorageStates()
	if len(got) != 1 {
		t.Fatalf("expected 1 entry after dedup, got %d: %#v", len(got), got)
	}
	if got[0].NodeID != "node-x" || got[0].NodeIP != "10.0.0.9" || got[0].Mode != "warn" {
		t.Fatalf("unexpected entry: %#v", got[0])
	}
}

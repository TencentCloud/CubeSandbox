// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"strings"
	"sync"
)

type SnapshotStorageState struct {
	NodeID        string
	NodeIP        string
	UsagePct      uint64
	Mode          string
	LastError     string
	LastUpdatedAt int64
}

var snapshotStorageStateCache = struct {
	sync.RWMutex
	byNode map[string]SnapshotStorageState
}{
	byNode: make(map[string]SnapshotStorageState),
}

func SetSnapshotStorageState(state SnapshotStorageState) {
	snapshotStorageStateCache.Lock()
	defer snapshotStorageStateCache.Unlock()
	if state.NodeID != "" {
		snapshotStorageStateCache.byNode[state.NodeID] = state
	}
	if state.NodeIP != "" {
		snapshotStorageStateCache.byNode[state.NodeIP] = state
	}
}

func GetSnapshotStorageState(nodeID, nodeIP string) (SnapshotStorageState, bool) {
	snapshotStorageStateCache.RLock()
	defer snapshotStorageStateCache.RUnlock()
	if nodeID != "" {
		if state, ok := snapshotStorageStateCache.byNode[nodeID]; ok {
			return state, true
		}
	}
	if nodeIP != "" {
		if state, ok := snapshotStorageStateCache.byNode[nodeIP]; ok {
			return state, true
		}
	}
	return SnapshotStorageState{}, false
}

// ResetSnapshotStorageStateCacheForTest clears the in-process snapshot
// storage state cache. It is exported solely so cross-package tests (for
// example, templatecenter) can isolate cache state between cases. Production
// code MUST NOT call this.
func ResetSnapshotStorageStateCacheForTest() {
	snapshotStorageStateCache.Lock()
	snapshotStorageStateCache.byNode = make(map[string]SnapshotStorageState)
	snapshotStorageStateCache.Unlock()
}

// ListSnapshotStorageStates returns all currently cached snapshot storage
// states, deduplicated by (NodeID, NodeIP). It is intended for the snapshot
// reconciler to keep retrying nodes whose last refresh ended in failure even
// when the healthy-node list (backed by redis) is temporarily empty.
func ListSnapshotStorageStates() []SnapshotStorageState {
	snapshotStorageStateCache.RLock()
	defer snapshotStorageStateCache.RUnlock()
	if len(snapshotStorageStateCache.byNode) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(snapshotStorageStateCache.byNode))
	out := make([]SnapshotStorageState, 0, len(snapshotStorageStateCache.byNode))
	for _, state := range snapshotStorageStateCache.byNode {
		key := state.NodeID + "|" + state.NodeIP
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, state)
	}
	return out
}

func IsSnapshotStorageWriteAllowed(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "healthy", "warn":
		return true
	default:
		return false
	}
}

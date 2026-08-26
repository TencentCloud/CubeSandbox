// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
)

// newTestLocal returns a local with empty caches, no DB, for testing
// syncAllFromDB's empty-streak logic.
func newTestLocal() *local {
	return &local{
		cache:                 cache.New(0, 0),
		imageCache:            cache.New(0, 0),
		templateNodeCache:     cache.New(0, 0),
		sortedNodesByClusters: make(map[string]node.NodeList),
	}
}

// withExternalNodeLoader swaps the package-level loader, restoring it on cleanup.
func withExternalNodeLoader(t *testing.T, loader func(context.Context) ([]*node.Node, error)) {
	t.Helper()
	orig := externalNodeLoader
	externalNodeLoader = loader
	t.Cleanup(func() { externalNodeLoader = orig })
}

func emptyLoader() func(context.Context) ([]*node.Node, error) {
	return func(context.Context) ([]*node.Node, error) { return nil, nil }
}

func TestSyncAllFromDB_EmptyStreakSkipsEviction(t *testing.T) {
	l := newTestLocal()
	// Seed nodes so we can prove the skip-eviction branch protects existing
	// cache contents, not just that the counter accumulates.
	l.cache.SetDefault("node-1", &node.Node{InsID: "node-1"})
	l.cache.SetDefault("node-2", &node.Node{InsID: "node-2"})

	withExternalNodeLoader(t, emptyLoader())
	ctx := context.Background()

	for i := 0; i < emptySyncEvictThreshold-1; i++ {
		if err := l.syncAllFromDB(ctx, true); err != nil {
			t.Fatalf("syncAllFromDB: %v", err)
		}
	}
	assert.Equal(t, int32(emptySyncEvictThreshold-1), l.emptySyncStreak.Load(),
		"streak accumulates below threshold and eviction is skipped")
	assert.Equal(t, 2, len(l.cache.Items()),
		"nodes seeded before sustained empty syncs must survive the skip-eviction branch")
}

func TestSyncAllFromDB_EmptyStreakEvictsAfterThreshold(t *testing.T) {
	l := newTestLocal()
	// Seed nodes so eviction is observable, not just a counter reset.
	l.cache.SetDefault("node-1", &node.Node{InsID: "node-1"})
	l.cache.SetDefault("node-2", &node.Node{InsID: "node-2"})
	assert.Equal(t, 2, len(l.cache.Items()), "precondition: cache seeded")

	// Stub delNodeCache side-effects so eviction runs without external deps.
	patches := gomonkey.NewPatches()
	patches.ApplyFunc(SyncNodeTemplates, func(context.Context, string, []string) {})
	patches.ApplyFunc(grpcconn.CloseWorkerConn, func(string) {})
	defer patches.Reset()

	withExternalNodeLoader(t, emptyLoader())
	ctx := context.Background()

	for i := 0; i < emptySyncEvictThreshold; i++ {
		if err := l.syncAllFromDB(ctx, true); err != nil {
			t.Fatalf("syncAllFromDB: %v", err)
		}
	}
	assert.Equal(t, int32(0), l.emptySyncStreak.Load(),
		"streak resets to 0 after eviction at threshold")
	assert.Equal(t, 0, len(l.cache.Items()),
		"eviction should remove cached nodes at threshold")
}

func TestSyncAllFromDB_EmptyStreakEvictsEmptyCache(t *testing.T) {
	l := newTestLocal()
	// scale-to-zero: cache is already empty, so eviction is a no-op. The
	// eviction path must still complete cleanly (streak reset, no panic).
	withExternalNodeLoader(t, emptyLoader())
	ctx := context.Background()

	for i := 0; i < emptySyncEvictThreshold; i++ {
		if err := l.syncAllFromDB(ctx, true); err != nil {
			t.Fatalf("syncAllFromDB: %v", err)
		}
	}
	assert.Equal(t, int32(0), l.emptySyncStreak.Load(),
		"streak resets after eviction")
	assert.Equal(t, 0, len(l.cache.Items()),
		"empty cache stays empty after eviction")
}

func TestSyncAllFromDB_NonEmptyResetsStreak(t *testing.T) {
	l := newTestLocal()
	calls := 0
	withExternalNodeLoader(t, func(context.Context) ([]*node.Node, error) {
		calls++
		if calls == 3 {
			return []*node.Node{{InsID: "node-1"}}, nil // non-empty resets streak
		}
		return nil, nil
	})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := l.syncAllFromDB(ctx, true); err != nil {
			t.Fatalf("syncAllFromDB: %v", err)
		}
	}
	// streak: 1, 2, 0 (non-empty), 1, 2
	assert.Equal(t, int32(2), l.emptySyncStreak.Load(), "streak resets on non-empty then accumulates")
}

func TestSyncAllFromDB_LoadErrorResetsStreak(t *testing.T) {
	l := newTestLocal()
	// Seed a non-zero streak, then fail; a load error must reset it to 0.
	loadErr := errors.New("cubeops unreachable")
	calls := 0
	withExternalNodeLoader(t, func(context.Context) ([]*node.Node, error) {
		calls++
		if calls >= 4 { // 3 empty syncs build streak=3, the 4th call fails
			return nil, loadErr
		}
		return nil, nil
	})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := l.syncAllFromDB(ctx, true); err != nil {
			t.Fatalf("seed sync %d: %v", i, err)
		}
	}
	assert.Equal(t, int32(3), l.emptySyncStreak.Load(), "streak should accumulate from empty syncs before error")

	if err := l.syncAllFromDB(ctx, true); err == nil {
		t.Fatal("expected error from loader")
	}
	assert.Zero(t, l.emptySyncStreak.Load(), "load error must reset streak to 0")
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/nodehealth"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
)

// useTestLocal installs a fresh local as the package global for AbsentSince.
func useTestLocal(t *testing.T) *local {
	t.Helper()
	orig := l
	loc := newTestLocal()
	l = loc
	t.Cleanup(func() { l = orig })

	patches := gomonkey.NewPatches()
	patches.ApplyFunc(grpcconn.CloseWorkerConn, func(string) {})
	t.Cleanup(patches.Reset)
	return loc
}

// markSynced marks the node view as freshly synced, as a successful sync does.
func markSynced(loc *local) {
	loc.syncedAt.Store(time.Now().UnixNano())
	loc.synced.Store(true)
}

// observedNode caches a node whose receive time last changed at observedAt.
func observedNode(loc *local, id string, observedAt time.Time, opts ...func(*node.Node)) {
	n := &node.Node{InsID: id, HeartbeatReceivedAt: time.Unix(1700000000, 0), HeartbeatObservedAt: observedAt}
	for _, o := range opts {
		o(n)
	}
	loc.addNodeCache(n)
}

// Healthy and UnhealthyReason come from the cubelet clock and must not decide
// reachability: a stopped cubelet with a clock ahead still reports Healthy.
func TestUnreachableSinceDependsOnObservedHeartbeatOnly(t *testing.T) {
	observedAt := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		healthy bool
		reason  string
	}{
		{"healthy", true, ""},
		{"reported not ready", false, nodehealth.ReasonReportedNotReady},
		{"heartbeat expired", false, nodehealth.ReasonHeartbeatExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := useTestLocal(t)
			markSynced(loc)
			observedNode(loc, "node-1", observedAt, func(n *node.Node) {
				n.Healthy, n.UnhealthyReason = tc.healthy, tc.reason
				n.MetaDataUpdateAt = time.Now().Add(2 * time.Hour)
			})
			if since, ok := UnreachableSince("node-1"); !ok || !since.Equal(observedAt) {
				t.Fatalf("got (%s, %v), want (%s, true)", since, ok, observedAt)
			}
		})
	}
}

// CubeOps timestamps are only compared with each other: a receive time far
// ahead of or behind CubeMaster's clock stays fresh while it keeps changing.
func TestUnreachableSinceIgnoresControlPlaneClockSkew(t *testing.T) {
	for _, skew := range []time.Duration{time.Hour, -time.Hour} {
		loc := useTestLocal(t)
		markSynced(loc)
		received := time.Now().Add(skew)
		loc.addNodeCache(&node.Node{InsID: "node-1", HeartbeatReceivedAt: received})
		if err := loc.updateNodeFromMetaData(&node.Node{InsID: "node-1", HeartbeatReceivedAt: received.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
		if since, ok := UnreachableSince("node-1"); !ok || time.Since(since) > time.Minute {
			t.Fatalf("skew %s: a changing receive time must stay fresh, got (%s, %v)", skew, since, ok)
		}
	}
}

// An unchanged receive time keeps the first observation, so a stopped node ages;
// a nanosecond-to-second rebuild of the same heartbeat is not a new one.
func TestUpdateKeepsObservationUntilReceiveSecondChanges(t *testing.T) {
	loc := useTestLocal(t)
	received := time.Unix(1700000000, 123456789)
	old := time.Now().Add(-time.Hour)
	loc.addNodeCache(&node.Node{InsID: "node-1", HeartbeatReceivedAt: received, HeartbeatObservedAt: old})

	for _, same := range []time.Time{received, time.Unix(received.Unix(), 0)} {
		if err := loc.updateNodeFromMetaData(&node.Node{InsID: "node-1", HeartbeatReceivedAt: same}); err != nil {
			t.Fatal(err)
		}
		if n, _ := GetNode("node-1"); !n.HeartbeatObservedAt.Equal(old) {
			t.Fatalf("receive time %s must not refresh the observation: %s", same, n.HeartbeatObservedAt)
		}
	}
	before := time.Now()
	if err := loc.updateNodeFromMetaData(&node.Node{InsID: "node-1", HeartbeatReceivedAt: received.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if n, _ := GetNode("node-1"); n.HeartbeatObservedAt.Before(before) {
		t.Fatalf("a new receive second must refresh the observation: %s", n.HeartbeatObservedAt)
	}
}

// A node first seen (startup or re-join) starts aging from CubeMaster's now,
// never from an old CubeOps receive time.
func TestAddNodeCacheStartsObservationNow(t *testing.T) {
	loc := useTestLocal(t)
	markSynced(loc)
	before := time.Now()
	loc.addNodeCache(&node.Node{InsID: "node-1", HeartbeatReceivedAt: time.Now().Add(-time.Hour)})
	if since, ok := UnreachableSince("node-1"); !ok || since.Before(before) {
		t.Fatalf("first observation must be now, got (%s, %v)", since, ok)
	}
}

// Changes during a sync gap were unobservable, so the first sync after a gap
// over viewStaleAfter restarts aging for every node.
func TestSyncAfterGapRestartsObservation(t *testing.T) {
	loc := useTestLocal(t)
	observedNode(loc, "node-1", time.Now().Add(-time.Hour))
	loc.syncedAt.Store(time.Now().Add(-viewStaleAfter - 10*time.Minute).UnixNano())
	loc.synced.Store(true)

	loc.markSyncSucceeded(time.Now().Add(-2 * time.Minute))
	if since, ok := UnreachableSince("node-1"); !ok || time.Since(since) > 3*time.Minute {
		t.Fatalf("a resume after a gap must bound the observation, got (%s, %v)", since, ok)
	}
	resumed := loc.resumedAt.Load()
	loc.markSyncSucceeded(time.Now())
	if loc.resumedAt.Load() != resumed {
		t.Fatal("a sync without a gap must not move resumedAt")
	}
}

// Sync rewrites the cached node every second while cleanup reads it; under -race
// the verdict must read both stamps under the writers' lock.
func TestUnreachableSinceConcurrentWithSync(t *testing.T) {
	loc := useTestLocal(t)
	markSynced(loc)
	start := time.Now()
	base := time.Unix(1700000000, 0)
	loc.addNodeCache(&node.Node{InsID: "node-1", HeartbeatReceivedAt: base})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 2000; i++ {
			_ = loc.updateNodeFromMetaData(&node.Node{InsID: "node-1", HeartbeatReceivedAt: base.Add(time.Duration(i) * time.Second)})
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		if since, ok := UnreachableSince("node-1"); !ok || since.Before(start) {
			t.Fatalf("got (%s, %v), want a time after %s", since, ok, start)
		}
	}
}

func TestUnreachableSinceStaleViewFailsClosed(t *testing.T) {
	loc := useTestLocal(t)
	observedNode(loc, "node-1", time.Now().Add(-time.Hour))
	loc.synced.Store(true)

	loc.syncedAt.Store(time.Now().Add(-viewStaleAfter - time.Second).UnixNano())
	if _, ok := UnreachableSince("node-1"); ok {
		t.Fatal("a view older than viewStaleAfter must not conclude unreachability")
	}
	loc.syncedAt.Store(time.Now().Add(-viewStaleAfter + time.Minute).UnixNano())
	if _, ok := UnreachableSince("node-1"); !ok {
		t.Fatal("a view within viewStaleAfter must still conclude unreachability")
	}
}

func TestUnreachableSinceFailsClosed(t *testing.T) {
	loc := useTestLocal(t)
	if _, ok := UnreachableSince("node-1"); ok {
		t.Fatal("an unsuccessful initial sync must not conclude unreachability")
	}
	markSynced(loc)
	loc.addNodeCache(&node.Node{
		InsID: "node-1", Healthy: false, UnhealthyReason: nodehealth.ReasonHeartbeatExpired,
		HeartbeatObservedAt: time.Now().Add(-time.Hour),
	})
	if _, ok := UnreachableSince("node-1"); ok {
		t.Fatal("a zero heartbeat timestamp must fail closed")
	}
	if _, ok := UnreachableSince(""); ok {
		t.Fatal("an empty node id must fail closed")
	}
}

func TestWarnOnMassHeartbeatStallRateLimited(t *testing.T) {
	loc := useTestLocal(t)
	markSynced(loc)
	observedNode(loc, "a", time.Now().Add(-time.Hour))
	observedNode(loc, "b", time.Now().Add(-time.Hour))
	observedNode(loc, "c", time.Now())
	ids := map[string]struct{}{"a": {}, "b": {}, "c": {}}

	loc.warnOnMassHeartbeatStall(context.Background(), ids)
	first := loc.stallWarnedAt.Load()
	if first == 0 {
		t.Fatal("two of three stalled nodes must warn")
	}
	loc.warnOnMassHeartbeatStall(context.Background(), ids)
	if loc.stallWarnedAt.Load() != first {
		t.Fatal("the warning must be rate-limited to once a minute")
	}

	quiet := useTestLocal(t)
	observedNode(quiet, "a", time.Now().Add(-time.Hour))
	observedNode(quiet, "b", time.Now())
	quiet.warnOnMassHeartbeatStall(context.Background(), map[string]struct{}{"a": {}, "b": {}})
	if quiet.stallWarnedAt.Load() != 0 {
		t.Fatal("half the nodes stalled is not a mass stall")
	}
}

// A cache entry that is not a *node.Node must not fall back to view absence:
// the node is still registered, so its own heartbeat decides reachability.
func TestUnreachableSinceCacheEntryDoesNotFallBackToAbsence(t *testing.T) {
	loc := useTestLocal(t)
	markSynced(loc)
	loc.cache.SetDefault("node-1", "not-a-node")
	if _, ok := UnreachableSince("node-1"); ok {
		t.Fatal("a malformed cache entry must fail closed")
	}
}

func TestAbsentSince_NotSyncedConcludesNothing(t *testing.T) {
	useTestLocal(t)
	_, ok := AbsentSince("node-1")
	assert.False(t, ok, "before the first successful sync nothing may be concluded")
}

func TestAbsentSince_EmptyIDConcludesNothing(t *testing.T) {
	loc := useTestLocal(t)
	loc.synced.Store(true)
	_, ok := AbsentSince("")
	assert.False(t, ok)
}

func TestAbsentSince_PresentNodeIsNotAbsent(t *testing.T) {
	loc := useTestLocal(t)
	loc.synced.Store(true)
	// Health must not matter: an unhealthy but registered node is present.
	loc.addNodeCache(&node.Node{InsID: "node-1", IP: "10.0.0.1", Healthy: false})
	_, ok := AbsentSince("node-1")
	assert.False(t, ok)
}

func TestAbsentSince_UnknownNodeTimedFromFirstQuery(t *testing.T) {
	loc := useTestLocal(t)
	loc.synced.Store(true)

	first, ok := AbsentSince("never-seen")
	assert.True(t, ok)
	time.Sleep(5 * time.Millisecond)
	second, ok := AbsentSince("never-seen")
	assert.True(t, ok)
	assert.True(t, first.Equal(second), "absence start must stay at the first observation")
}

func TestAbsentSince_EvictionThenReappearanceResets(t *testing.T) {
	loc := useTestLocal(t)
	loc.synced.Store(true)
	n := &node.Node{InsID: "node-1", IP: "10.0.0.1", Healthy: true}
	loc.addNodeCache(n)

	before := time.Now()
	loc.delNodeCache(context.Background(), n)
	since, ok := AbsentSince("node-1")
	assert.True(t, ok)
	assert.False(t, since.Before(before), "eviction time starts the absence")

	loc.addNodeCache(n)
	_, ok = AbsentSince("node-1")
	assert.False(t, ok, "reappearance clears the absence")

	// A second disappearance restarts the clock instead of inheriting the old one.
	time.Sleep(5 * time.Millisecond)
	second := time.Now()
	loc.delNodeCache(context.Background(), n)
	since, ok = AbsentSince("node-1")
	assert.True(t, ok)
	assert.False(t, since.Before(second))
}

func TestAbsentSince_SyncEvictsMissingNode(t *testing.T) {
	loc := useTestLocal(t)
	loc.cache.SetDefault("node-1", &node.Node{InsID: "node-1"})
	loc.cache.SetDefault("node-2", &node.Node{InsID: "node-2"})

	withExternalNodeLoader(t, func(context.Context) ([]*node.Node, error) {
		return []*node.Node{{InsID: "node-2"}}, nil
	})
	if err := loc.syncAllFromDB(context.Background(), true); err != nil {
		t.Fatalf("syncAllFromDB: %v", err)
	}
	_, ok := AbsentSince("node-1")
	assert.True(t, ok, "node dropped from a partial response is recorded absent")
	_, ok = AbsentSince("node-2")
	assert.False(t, ok)
}

func TestAbsentSince_LoadFailureDoesNotMarkSynced(t *testing.T) {
	loc := useTestLocal(t)
	withExternalNodeLoader(t, func(context.Context) ([]*node.Node, error) {
		return nil, errors.New("cubeops unreachable")
	})
	if err := loc.syncAllFromDB(context.Background(), true); err == nil {
		t.Fatal("expected loader error")
	}
	_, ok := AbsentSince("node-1")
	assert.False(t, ok, "a failed sync must not make absence conclusions possible")
	assert.Zero(t, loc.syncedAt.Load(), "a failed sync must not refresh the view time")
}

func TestSyncAllFromDB_SuccessRefreshesViewTime(t *testing.T) {
	loc := useTestLocal(t)
	withExternalNodeLoader(t, emptyLoader())
	before := time.Now().UnixNano()
	if err := loc.syncAllFromDB(context.Background(), true); err != nil {
		t.Fatalf("syncAllFromDB: %v", err)
	}
	assert.GreaterOrEqual(t, loc.syncedAt.Load(), before, "an empty view is still a successful sync")
}

func TestAbsentSince_EmptyViewIsValidAfterSync(t *testing.T) {
	loc := useTestLocal(t)
	withExternalNodeLoader(t, emptyLoader())
	if err := loc.syncAllFromDB(context.Background(), true); err != nil {
		t.Fatalf("syncAllFromDB: %v", err)
	}
	_, ok := AbsentSince("last-node")
	assert.True(t, ok, "a legitimately empty view still lets absence accumulate")
}

// TestAbsentSince_CachePresenceAndAbsenceAreOneCriticalSection deterministically
// proves addNodeCache blocks on the lock AbsentSince uses, so no mid-update read.
func TestAbsentSince_CachePresenceAndAbsenceAreOneCriticalSection(t *testing.T) {
	loc := useTestLocal(t)
	loc.synced.Store(true)
	n := &node.Node{InsID: "node-1", IP: "10.0.0.1"}

	loc.absence.mu.Lock()
	done := make(chan struct{})
	go func() {
		loc.addNodeCache(n)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("addNodeCache must not complete while the absence lock is held elsewhere")
	case <-time.After(20 * time.Millisecond):
		// Still blocked, as expected: cache write + absence clear are one section.
	}

	loc.absence.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("addNodeCache did not complete after the lock was released")
	}

	// Nothing can have observed an intermediate state: the node is present
	// and clean, with no stale absence record.
	_, ok := AbsentSince("node-1")
	assert.False(t, ok)
}

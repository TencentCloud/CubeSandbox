// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
)

// pendingRestoreTTL bounds how long a deferred tombstone restore stays
// recorded. It is generous because the only consumer is the next lifecycle
// operation on that sandbox, which normally arrives within seconds (the user
// retries the Resume); past the TTL the record is dropped rather than applied,
// because by then Master and the node have both moved on and restoring a
// long-stale tombstone could resurrect a sandbox whose pause package has since
// been reclaimed.
const pendingRestoreTTL = 24 * time.Hour

// pendingPausedRestore is a tombstone restore a failover could not perform
// because it never got the sandbox's lifecycle lock within its wait budget.
type pendingPausedRestore struct {
	snapshot        *cubeboxstore.CubeBox
	debrisCreatedAt int64
	recordedAt      time.Time
}

// pausedRestoreRegistry holds the deferred restores, keyed by sandbox ID. The
// mutex only guards the map; the store work happens outside it.
type pausedRestoreRegistry struct {
	sync.Mutex
	entries map[string]pendingPausedRestore
}

// record registers (or replaces) a deferred restore and prunes expired ones.
func (r *pausedRestoreRegistry) record(sandboxID string, snapshot *cubeboxstore.CubeBox, debrisCreatedAt int64, now time.Time) {
	if r == nil || sandboxID == "" || snapshot == nil {
		return
	}
	r.Lock()
	defer r.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]pendingPausedRestore)
	}
	for id, entry := range r.entries {
		if now.Sub(entry.recordedAt) > pendingRestoreTTL {
			delete(r.entries, id)
		}
	}
	r.entries[sandboxID] = pendingPausedRestore{
		snapshot:        snapshot,
		debrisCreatedAt: debrisCreatedAt,
		recordedAt:      now,
	}
}

// take removes and returns the deferred restore for sandboxID, if any.
func (r *pausedRestoreRegistry) take(sandboxID string) (pendingPausedRestore, bool) {
	if r == nil || sandboxID == "" {
		return pendingPausedRestore{}, false
	}
	r.Lock()
	defer r.Unlock()
	entry, ok := r.entries[sandboxID]
	if ok {
		delete(r.entries, sandboxID)
	}
	return entry, ok
}

// len is the test/diagnostics view of the registry size.
func (r *pausedRestoreRegistry) len() int {
	if r == nil {
		return 0
	}
	r.Lock()
	defer r.Unlock()
	return len(r.entries)
}

// recordPendingPausedRestore defers a tombstone restore the failover could not
// serialize. See pausedRestoreRegistry and completePendingPausedRestore.
func (l *local) recordPendingPausedRestore(sandboxID string, snapshot *cubeboxstore.CubeBox, debrisCreatedAt int64) {
	l.pendingPausedRestores.record(sandboxID, snapshot, debrisCreatedAt, time.Now())
}

// completePendingPausedRestore applies a restore a previous failed
// Create-from-pause had to defer, and reports whether it applied one.
//
// It must be called while holding the sandbox's lifecycle lock — service.Create
// does so for a resume, which is the operation that needs the tombstone back —
// so the write cannot interleave with another lifecycle operation on the same
// sandbox. The point of the deferral is self-healing: a rollback that never
// got the lock leaves this attempt's debris row in the store, and a debris row
// makes every later retry fail closed as "already exists" (the resume path has
// no Master compensation to clean it up), so without this the sandbox stays
// bricked until the user deletes it. Completing the restore first lets the
// retry evaluate the tombstone and run the normal shim + quota gates.
//
// The same value identity the rollback uses guards the deferred write: a row
// that is gone, or whose CreatedAt belongs to a newer attempt, is left alone.
func (l *local) completePendingPausedRestore(ctx context.Context, sandboxID string) bool {
	entry, ok := l.pendingPausedRestores.take(sandboxID)
	if !ok {
		return false
	}
	if time.Since(entry.recordedAt) > pendingRestoreTTL {
		log.G(ctx).Warnf("dropping a deferred tombstone restore for %s recorded %s ago", sandboxID, time.Since(entry.recordedAt))
		return false
	}
	cur, getErr := l.cubeboxManger.Get(ctx, sandboxID)
	if getErr != nil || cur == nil {
		log.G(ctx).Warnf("deferred tombstone restore for %s is moot: no local row", sandboxID)
		return false
	}
	if entry.debrisCreatedAt != 0 && cur.CreatedAt != entry.debrisCreatedAt {
		log.G(ctx).Warnf("deferred tombstone restore for %s skipped: the row belongs to a newer attempt (CreatedAt %d, deferred %d)",
			sandboxID, cur.CreatedAt, entry.debrisCreatedAt)
		return false
	}
	snap := entry.snapshot
	snap.Lock()
	snap.UserMarkDeletedTime = nil
	snap.DeleteRequestID = ""
	snap.Unlock()
	if saveErr := l.cubeboxManger.Save(ctx, snap, cubes.WithNoEvent); saveErr != nil {
		log.G(ctx).Errorf("deferred tombstone restore for %s failed: %v", sandboxID, saveErr)
		return false
	}
	log.G(ctx).Warnf("deferred tombstone restore for %s completed before the retry", sandboxID)
	return true
}

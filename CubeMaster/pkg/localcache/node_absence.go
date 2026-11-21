// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"sync"
	"time"
)

// absenceTracker records since when a node id has been missing (cleared on
// return, never evicted); its mutex also serialises cache add/delete with it.
type absenceTracker struct {
	mu    sync.Mutex
	since map[string]time.Time
}

// withLock runs fn while holding the tracker's lock; fn reads presence state and
// updates the absence map atomically, so callers never expose lock/unlock pairs.
func (a *absenceTracker) withLock(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn()
}

// noteAbsentLocked records the first observation of absence; later calls
// keep it. Must be called with the tracker's lock held (see withLock).
func (a *absenceTracker) noteAbsentLocked(nodeID string, now time.Time) time.Time {
	if t, ok := a.since[nodeID]; ok {
		return t
	}
	if a.since == nil {
		a.since = make(map[string]time.Time)
	}
	a.since[nodeID] = now
	return now
}

// clearLocked drops any absence record for nodeID. Must be called with the
// tracker's lock held (see withLock).
func (a *absenceTracker) clearLocked(nodeID string) {
	delete(a.since, nodeID)
}

// AbsentSince reports since when nodeID has been missing from the node view;
// ok is false while present or before any successful sync. Health is ignored.
func AbsentSince(nodeID string) (since time.Time, ok bool) {
	if nodeID == "" || !l.synced.Load() {
		return time.Time{}, false
	}
	l.absence.withLock(func() {
		if _, present := l.cache.Get(nodeID); present {
			// Defensive: addNodeCache already clears it under the same lock;
			// never leave a stale entry for a node we can see is back.
			l.absence.clearLocked(nodeID)
			return
		}
		ok = true
		since = l.absence.noteAbsentLocked(nodeID, time.Now())
	})
	return since, ok
}

// viewStaleAfter exceeds the worst-case sync (6 x 10s timeouts + 31s backoff);
// an older view cannot prove a registered node stopped sending heartbeats.
const viewStaleAfter = 3 * time.Minute

// UnreachableSince returns when a registered node's last heartbeat reached
// CubeOps, ignoring Healthy; an unknown receive time or stale view fails closed.
func UnreachableSince(nodeID string) (time.Time, bool) {
	if nodeID == "" || !l.synced.Load() {
		return time.Time{}, false
	}
	n, present := GetNode(nodeID)
	if !present {
		return AbsentSince(nodeID)
	}
	now := time.Now()
	if n == nil || n.HeartbeatReceivedAt.IsZero() ||
		now.Sub(time.Unix(0, l.syncedAt.Load())) > viewStaleAfter {
		return time.Time{}, false
	}
	// A future control-plane timestamp must not shorten the grace period.
	if n.HeartbeatReceivedAt.After(now) {
		return now, true
	}
	return n.HeartbeatReceivedAt, true
}

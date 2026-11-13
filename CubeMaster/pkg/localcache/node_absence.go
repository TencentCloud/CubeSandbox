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

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
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

// UnreachableSince returns when CubeMaster last saw a registered node's CubeOps
// receive time change; only local clocks are compared, so skew cannot matter.
func UnreachableSince(nodeID string) (time.Time, bool) {
	if nodeID == "" || !l.synced.Load() {
		return time.Time{}, false
	}
	received, observed, present := l.heartbeatTimes(nodeID)
	if !present {
		return AbsentSince(nodeID)
	}
	if received.IsZero() || observed.IsZero() ||
		time.Since(time.Unix(0, l.syncedAt.Load())) > viewStaleAfter {
		return time.Time{}, false
	}
	return l.heartbeatObservedAt(observed), true
}

// heartbeatTimes reads both stamps under lockMetaData, which every writer holds;
// Clone reads unlocked and could observe a torn time.Time.
func (l *local) heartbeatTimes(nodeID string) (received, observed time.Time, present bool) {
	v, present := l.cache.Get(nodeID)
	n, ok := v.(*node.Node)
	if !present || !ok || n == nil {
		return time.Time{}, time.Time{}, present
	}
	l.lockMetaData.Lock()
	defer l.lockMetaData.Unlock()
	return n.HeartbeatReceivedAt, n.HeartbeatObservedAt, true
}

// heartbeatObservedAt never predates the last resume from a stale view: changes
// that happened while CubeMaster could not sync were not observable.
func (l *local) heartbeatObservedAt(observed time.Time) time.Time {
	if resumed := l.resumedAt.Load(); resumed != 0 && time.Unix(0, resumed).After(observed) {
		return time.Unix(0, resumed)
	}
	return observed
}

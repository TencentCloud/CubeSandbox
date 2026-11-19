// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import "sync"

// resumeReservationLedger tracks the node quota held by resumes that admission
// has approved but whose sandbox rows are not yet (or no longer) visible to
// aggregateAllocated. Without it, two concurrent resumes of different sandboxes
// — which hold different per-sandbox lifecycle locks and therefore run
// admission truly in parallel — each read the same pre-resume snapshot of the
// node and both pass: the concurrent repro in #2042 (quota 8192, used 6144,
// two resumes of 2048 each ending with 10240 committed on an 8 GiB node).
//
// The embedded mutex is the node-level admission lock and deliberately the
// only lock the decision crosses: admitResume holds it while it reads the
// store aggregate, checks the quota, and registers the demand, so
// read-check-register is one atomic step with respect to every other
// admission. It is a short in-memory critical section (an O(n) scan of the
// node's rows); the slow work — the create flow allocating network/volume/
// cgroup and booting the VM — runs outside it, so resumes stay concurrent and
// only the gate itself serializes.
//
// What an entry holds: the sandbox's FULL quota demand, not the incremental
// r fraction the admission check compares against. Walk the create-from-pause
// timeline for a sandbox with full demand F under release ratio r (its PAUSED
// tombstone contributes T = (1-r)·F to the aggregate):
//
//	admission → tombstone Delete : store T + reserved F  (over by T, conservative)
//	tombstone Delete → flow Save : store 0 + reserved F  (exact)
//	flow Save → release          : store F + reserved F  (over by F, conservative)
//
// Reserving only the incremental r·F instead would make the Delete → Save
// window under-count by T — the one direction that can admit a resume that
// does not fit, which is exactly what this ledger exists to prevent. Every
// skew above is therefore deliberately one-sided: the ledger can only reject
// a resume that would have fit, never admit one that would not. At r = 1
// (full release, the #2042 configuration) T = 0 and the reservation equals the
// incremental demand anyway.
//
// Lifecycle of an entry: registered at admission, dropped by the release func
// the caller defers — on success after the flow's Save has folded the demand
// into aggregateAllocated, on failure/panic/cancellation simply because the
// demand goes away. Entries are pure in-memory state: if the cubelet dies the
// unsaved rows die with it, and the store rebuilt at startup agrees with an
// empty ledger, so there is nothing to persist or recover.
type resumeReservationLedger struct {
	sync.Mutex
	entries map[string]*resumeReservation
}

// noopRelease is returned instead of a real release func when no reservation
// was registered (policy off, request is not a paused replacement, or the
// resume was rejected), so every caller can defer its release unconditionally.
func noopRelease() {}

// resumeReservation is one admitted-but-not-yet-settled resume's full quota
// demand, in the same units resumeQuotaCheck uses (MB / millicores).
type resumeReservation struct {
	memMB    int64
	cpuMilli int64
}

// totalsLocked sums the in-flight reservations. The entry count is bounded by
// the number of concurrently-resuming sandboxes on this node, so the scan is
// trivial. Callers must hold the lock.
func (l *resumeReservationLedger) totalsLocked() (memMB, cpuMilli int64) {
	for _, r := range l.entries {
		memMB += r.memMB
		cpuMilli += r.cpuMilli
	}
	return memMB, cpuMilli
}

// totals is the self-locking read used by tests and diagnostics.
func (l *resumeReservationLedger) totals() (memMB, cpuMilli int64) {
	l.Lock()
	defer l.Unlock()
	return l.totalsLocked()
}

// reserveLocked records the demand and returns its release func. Callers must
// hold the lock — admission does, as part of the same critical section that
// checked the quota, so no other admission can observe a gap between check and
// register. The returned release is idempotent and safe to call from any
// goroutine; it compares pointer identity, so a stale release (a deferred one
// firing late, or a double defer) can never drop a newer reservation
// registered for the same sandbox ID.
func (l *resumeReservationLedger) reserveLocked(id string, memMB, cpuMilli int64) func() {
	if l.entries == nil {
		l.entries = make(map[string]*resumeReservation)
	}
	r := &resumeReservation{memMB: memMB, cpuMilli: cpuMilli}
	l.entries[id] = r
	return func() {
		l.Lock()
		defer l.Unlock()
		if l.entries[id] == r {
			delete(l.entries, id)
		}
	}
}

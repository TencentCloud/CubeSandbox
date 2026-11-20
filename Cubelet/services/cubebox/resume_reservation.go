// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

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
//	admission → flow Save : store T + reserved F  (over by T, conservative)
//	flow Save → release   : store F + reserved F  (over by F, conservative)
//
// There is no window where the store shows 0 for the sandbox: the replacement
// branch does not delete the tombstone, the flow's Save overwrites it (the
// store keys rows by ID), and that is also what makes a failed attempt safe —
// the rollback keeps the still-PAUSED row, so the user's retry finds it and
// is gated again (see createRollbackKeepsPausedTombstone).
//
// Reserving only the incremental r·F would be exact in the first window
// (T + r·F = F) but would depend on the tombstone staying visible until the
// Save; reserving F keeps every skew deliberately one-sided regardless of how
// the row's lifetime shifts: the ledger can only reject a resume that would
// have fit, never admit one that would not. At r = 1 (full release, the
// #2042 configuration) T = 0 and the reservation equals the incremental
// demand anyway.
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
//
// Precondition: admissions for one sandbox ID are serialized by the caller,
// and the serialization is guaranteed by Master's request shape rather than
// by the locking code structurally: service.Create holds that ID's
// sandboxLifecycleLocks entry exactly when the request carries BOTH
// cube.master.desired.sandbox.id and cube.master.pause.snapshot.id
// (resumeFromPauseSandboxID), and Master always sends the pair together for
// a same-node resume. The admission predicate itself (pausedReplaceTarget)
// fires on desired-id + a local PAUSED row, so a hypothetical Create with
// only the desired-id landing on a PAUSED row would reach admission without
// the lock; no caller sends that shape today (restart-armed creates reuse
// the annotation but skip the replacement branch via ReuseFor, and the
// restart manager refuses PAUSED rows), and the Update path's resumeLocked
// holds the same lock. Registering over a live entry is logged rather than
// ignored anyway: if a caller ever broke the shape assumption, the
// overwrite would silently degrade the ledger from double-counting
// protection to single-counting, and the log line is the only trace.
func (l *resumeReservationLedger) reserveLocked(id string, memMB, cpuMilli int64) func() {
	if l.entries == nil {
		l.entries = make(map[string]*resumeReservation)
	}
	if prev, dup := l.entries[id]; dup {
		log.G(context.Background()).Warnf("resume reservation for %s overwrites an in-flight reservation (mem %d→%d MB, cpu %d→%d milli); admissions for one sandbox must be serialized by its lifecycle lock",
			id, prev.memMB, memMB, prev.cpuMilli, cpuMilli)
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

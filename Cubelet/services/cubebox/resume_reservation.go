// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"sync"
)

// resumeReservationLedger tracks the node quota held by resumes that admission
// has approved but whose sandbox rows are not yet (or no longer) visible to
// aggregateAllocated. Without it, two concurrent resumes of different sandboxes
// — which hold different per-sandbox lifecycle locks and therefore run
// admission truly in parallel — each read the same pre-resume snapshot of the
// node and both pass: the concurrent repro in #2042 (quota 8192, used 6144,
// two resumes of 2048 each ending with 10240 committed on an 8 GiB node).
//
// The private mutex is the node-level admission lock and deliberately the
// only lock the decision crosses: admitResume holds it while it reads the
// cached aggregate, checks the quota, and registers the demand, so
// check-and-register is one atomic step with respect to every other
// admission. The aggregate read is served from a short-TTL cache, so the
// critical section is O(1) in the common case; the slow work — the create
// flow allocating network/volume/cgroup and booting the VM — runs outside it,
// so resumes stay concurrent and only the gate itself serializes.
//
// What an entry holds: the INCREMENTAL demand r·F this resume re-adds, not
// the sandbox's full quota F. Walk the create-from-pause timeline under
// release ratio r (the PAUSED tombstone contributes T = (1-r)·F to the
// aggregate):
//
//	admission → flow Save : store T + reserved r·F = F   (exact)
//	flow Save → release   : store F + reserved r·F       (over by r·F)
//
// Reserving the increment is exact pre-Save because the tombstone is
// guaranteed to stay visible until the flow's Save overwrites it in place:
// the replacement branch does not delete the row, and a failed attempt gets
// it back from the synchronous rollback — which tears the attempt down and
// saves the gate's snapshot over the row on the Create RPC's own goroutine,
// before the lock is released and before the deferred release drops this
// entry (see rollbackPausedReplace in service.go). There is no window where
// the store shows 0 for the sandbox, so every window above counts at least
// the sandbox's true commitment: the ledger can only reject a resume that
// would have fit, never admit one that would not. At r = 1 (full release,
// the #2042 configuration) T = 0 and the reservation equals F anyway.
//
// Lifecycle of an entry: registered at admission, dropped by the release func
// the caller defers — on success after the flow's Save has folded the demand
// into aggregateAllocated, on failure/panic/cancellation after the rollback
// has restored the tombstone. Entries are pure in-memory state: if the cubelet
// dies the unsaved rows die with it, and the store rebuilt at startup agrees
// with an empty ledger, so there is nothing to persist or recover.
//
// Serialization of admissions for one sandbox ID is structural, not assumed:
// service.Create is the only production entry (Update(resume) answers "not
// supported" — resume is a Master-driven Create-from-pause), and it holds the
// sandbox's lifecycle lock for the whole RPC whenever the request carries
// cube.master.desired.sandbox.id together with cube.master.pause.snapshot.id,
// which Master always sends as a pair for a same-node resume. A desired-id-only
// create that lands on a local PAUSED row — the one annotation wider than the
// lock's predicate — is rejected by gatePausedReplace before it can reach
// admission (no caller sends that shape today; restart-armed creates reuse the
// annotation but skip the replacement branch via ReuseFor and the restart
// manager refuses PAUSED rows), so every admission here runs under the lock.
// Registering over a live entry still overwrites with a warning rather than
// silently degrading from double-counting protection to single-counting.
type resumeReservationLedger struct {
	mu      sync.Mutex
	entries map[string]*resumeReservation
}

// noopRelease is returned instead of a real release func when no reservation
// was registered (policy off, request is not a paused replacement, or the
// resume was rejected), so every caller can defer its release unconditionally.
func noopRelease() {}

// resumeReservation is one admitted-but-not-yet-settled resume's incremental
// (released-fraction) demand, in the same units resumeQuotaCheck uses
// (MB / millicores).
type resumeReservation struct {
	memMB    int64
	cpuMilli int64
}

// totalsLocked sums the in-flight reservations. The entry count is bounded by
// the number of concurrently-resuming sandboxes on this node, so the scan is
// trivial. Callers must hold mu.
func (l *resumeReservationLedger) totalsLocked() (memMB, cpuMilli int64) {
	for _, r := range l.entries {
		memMB += r.memMB
		cpuMilli += r.cpuMilli
	}
	return memMB, cpuMilli
}

// totals is the self-locking read used by tests and diagnostics.
func (l *resumeReservationLedger) totals() (memMB, cpuMilli int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalsLocked()
}

// reserveLocked records the demand and returns its release func plus the
// reservation it replaced, if any. Callers must hold mu — admission does, as
// part of the same critical section that checked the quota, so no other
// admission can observe a gap between check and register. The returned
// release is idempotent and safe to call from any goroutine; it compares
// pointer identity, so a stale release (a deferred one firing late, or a
// double defer) can never drop a newer reservation registered for the same
// sandbox ID.
//
// The replaced entry is returned rather than logged here: the warning belongs
// to the caller's request context (requestID/trace) and must not run while
// the node-wide lock is held.
func (l *resumeReservationLedger) reserveLocked(id string, memMB, cpuMilli int64) (func(), *resumeReservation) {
	if l.entries == nil {
		l.entries = make(map[string]*resumeReservation)
	}
	prev := l.entries[id]
	r := &resumeReservation{memMB: memMB, cpuMilli: cpuMilli}
	l.entries[id] = r
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.entries[id] == r {
			delete(l.entries, id)
		}
	}, prev
}

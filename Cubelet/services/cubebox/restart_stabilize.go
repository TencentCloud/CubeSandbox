// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

// stabilizeSweep is how often the stabilizer looks for sandboxes whose streak
// has aged out.
//
// One second, matching statusReporter's flush period. The sweep is cheap: it
// walks the in-memory run map and calls store.Get, which is an indexer lookup
// with no I/O, and only a sandbox that is Running with a non-zero count takes
// the CubeBox lock. Waiting longer would be wrong rather than merely slower:
// the reset is lost once the sandbox exits and leaves Running, so a coarse
// sweep turns "was it up long enough?" into a race against the next crash.
const stabilizeSweep = time.Second

// startStabilizer launches the periodic sweep.
//
// It shares the lifetime model of statusReporter.run (statusreport.go): a plain
// ticker whose goroutine ends with the process. Giving it a cancellable context
// would need a shutdown hook this service does not have, which is more
// machinery than this job is worth.
func (m *restartMgr) startStabilizer() {
	if m == nil || m.store == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(stabilizeSweep)
		defer ticker.Stop()
		for range ticker.C {
			m.stabilizeOnce(time.Now())
		}
	}()
}

// stabilizeCandidate is a run snapshot taken under m.mu and read afterwards.
// It copies values rather than the *restartRun pointer on purpose: restarting
// and suspended are written under m.mu, so reading them from a shared pointer
// after unlocking would be a data race.
type stabilizeCandidate struct {
	id         string
	backoff    restartpolicy.Backoff
	restarting bool
	suspended  bool
}

// stabilizeOnce is the testable core of the stabilizer: no timers, no
// goroutines.
//
// It walks the run set rather than the store, because that set already holds
// exactly the sandboxes that have a restart policy, together with their parsed
// backoff. Using it avoids re-parsing the original request for every sandbox on
// every sweep.
func (m *restartMgr) stabilizeOnce(now time.Time) {
	m.mu.Lock()
	cands := make([]stabilizeCandidate, 0, len(m.runs))
	for id, run := range m.runs {
		if run == nil {
			continue
		}
		cands = append(cands, stabilizeCandidate{
			id:         id,
			backoff:    run.backoff,
			restarting: run.restarting,
			suspended:  run.suspended,
		})
	}
	m.mu.Unlock()

	for _, c := range cands {
		// A sandbox in the middle of a restart owns its own bookkeeping, and a
		// suspended one must not be evaluated at all.
		if c.restarting || c.suspended {
			continue
		}
		m.stabilizeOne(c.id, c.backoff, now)
	}
}

// stabilizeOne clears one sandbox's restart streak once its current run has
// been up for the configured stable duration.
//
// The cheap filters run without the lock so the common case -- a sandbox with
// nothing to clear -- costs a map lookup and two field reads. Reading those
// fields unlocked is safe here because they only ever decide whether to look
// again under the lock; a stale read can cost one extra acquisition, never a
// wrong write.
//
// IsPaused is deliberately last, and only reached by a sandbox that is Running,
// has a non-zero count, and whose window has already elapsed. It is the most
// expensive of the guards (it walks CubeBox.GetStatus, which resolves the main
// container several times and can log a stack trace when there is none), so
// paying for it on every sandbox every second would be wasteful.
func (m *restartMgr) stabilizeOne(sandboxID string, backoff restartpolicy.Backoff, now time.Time) {
	cb, err := m.store.Get(context.Background(), sandboxID)
	if err != nil || cb == nil {
		return
	}
	if cb.RestartState != restartpolicy.StateRunning || cb.RestartCount == 0 {
		return
	}

	// The closure keeps the critical section small and makes the unlock
	// unconditional without deferring it past m.observe, which takes cb.Lock
	// again through statusItem and would deadlock on the non-reentrant RWMutex.
	cb.Lock()
	cleared, saveErr := func() (bool, error) {
		// Re-check every guard under the lock: the prefilter above ran without
		// it, and a concurrent noteRestartFailure could have added a retry or
		// moved the state in between. Deciding on a stale count and then
		// writing would erase that retry, handing the sandbox a free attempt.
		if cb.RestartState != restartpolicy.StateRunning || cb.RestartCount == 0 {
			return false, nil
		}
		if !backoff.Stable(cb.StableSince, now) {
			return false, nil
		}
		if cb.IsPaused() {
			// A frozen guest was not demonstrably stable.
			return false, nil
		}
		prevCount, prevSince, prevSeq := cb.RestartCount, cb.StableSince, cb.StatusSeq
		applyRestartTransition(cb, restartTransition{
			resetCount:     true,
			setStableSince: true,
			stableSince:    now.UnixNano(),
			bumpSeq:        true,
		})
		if err := m.store.Save(context.Background(), cb); err != nil {
			// Roll back rather than report. This is the one write in the restart
			// path that hands a retry back to the user, so publishing a count the
			// node never persisted is worse than staying quiet: if the process
			// died before the next successful save, the persisted row would be
			// the truth while StatusSeq was already one ahead, and every later
			// report for this sandbox would be dropped as stale by both the
			// reporter and CubeMaster -- a split that never heals.
			// Restoring the count also lets the next sweep retry, because the
			// guard above needs a non-zero count to look again.
			cb.RestartCount, cb.StableSince, cb.StatusSeq = prevCount, prevSince, prevSeq
			return false, err
		}
		return true, nil
	}()
	cb.Unlock()

	if saveErr != nil {
		log.G(context.Background()).Warnf("restart: save stable reset for %s: %v", sandboxID, saveErr)
	}
	if !cleared {
		return
	}
	m.observe(sandboxID)
}

// backfillStableAnchor gives a row written before StableSince existed a usable
// anchor. Without it every sandbox that was already restarting at upgrade time
// keeps a permanently stale restartCount, because Stable refuses to guess from a
// zero anchor.
//
// The anchor is set to now, never to LastRestartAt. Adopting the old anchor
// would resurrect exactly the semantics this change removes: a sandbox whose
// last attempt was long ago would clear its streak immediately and gain a retry
// it did not earn. Anchoring at now is the conservative choice -- the worst case
// is that an upgraded sandbox waits one extra stable duration.
//
// This write deliberately does not bump StatusSeq or call observe. StableSince is
// internal: it is not reported to CubeMaster (statusreport.statusItem does not
// carry it), so bumping the sequence would only push an identical-looking status
// report for every sandbox on the node at startup.
func (m *restartMgr) backfillStableAnchor(cb *cubeboxstore.CubeBox) {
	if cb == nil || cb.StableSince > 0 {
		return
	}
	// Only states that can still be measured need an anchor. A BackOff row is
	// included because a spent budget commonly sits there; GaveUp is skipped by
	// the caller, and the terminal states never clear anyway.
	switch cb.RestartState {
	case restartpolicy.StateRunning, restartpolicy.StateBackOff:
	default:
		return
	}
	cb.Lock()
	applyRestartTransition(cb, restartTransition{
		setStableSince: true,
		stableSince:    time.Now().UnixNano(),
	})
	err := m.store.Save(context.Background(), cb)
	cb.Unlock()
	if err != nil {
		log.G(context.Background()).Warnf("restart: backfill stable anchor for %s: %v", cb.ID, err)
	}
}

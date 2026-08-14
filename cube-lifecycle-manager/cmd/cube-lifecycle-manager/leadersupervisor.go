// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// maxLeaderStintFails is how many consecutive fast-failing leader stints are
// tolerated before the process exits (see leaderSupervisor).
const maxLeaderStintFails = 3

// leaderSupervisor decides what happens when a leader stint ends. It exists
// to keep a *permanently* failing leader loop from hot-looping: without it a
// replica whose runLeaderLoops always fails fast (e.g. a bootstrap dependency
// is down while Redis itself is fine) would cycle elect → fail → step down →
// re-elect every renew interval forever — the process never exits, so the pod
// supervisor never restarts it and the only signal is log spam.
//
// Policy:
//   - a clean finish (nil / context.Canceled: leadership lost or shutdown)
//     resets the counter;
//   - a stint that survived at least stableAfter before failing counts as
//     healthy — a transient mid-run failure (Redis blip killing the stream
//     consumer) resets the counter too and just triggers a step-down;
//   - maxFails consecutive fast-failed stints → Record reports exit=true so
//     the process dies and Kubernetes (or any process supervisor) restarts it.
type leaderSupervisor struct {
	maxFails    int
	stableAfter time.Duration

	mu    sync.Mutex // a demoted stint may still be draining when the next one starts
	fails int
}

// newLeaderSupervisor builds a supervisor; stableAfter is typically the
// leader lease TTL.
func newLeaderSupervisor(maxFails int, stableAfter time.Duration) *leaderSupervisor {
	return &leaderSupervisor{maxFails: maxFails, stableAfter: stableAfter}
}

// Record notes the outcome of a leader stint and reports whether the process
// should exit instead of stepping down and re-electing.
func (s *leaderSupervisor) Record(err error, stint time.Duration) (exit bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		s.reset()
		return false
	}
	if stint >= s.stableAfter {
		s.reset()
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails++
	return s.fails >= s.maxFails
}

// Fails is the current consecutive fast-failure count (for logging).
func (s *leaderSupervisor) Fails() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fails
}

func (s *leaderSupervisor) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = 0
}

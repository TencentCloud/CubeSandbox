// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"sync"
	"time"
)

const (
	terminalTicketLimitWindow      = time.Minute
	terminalTicketLimitPerUser     = 12
	terminalSessionLimitPerUser    = 8
	terminalSessionLimitPerSandbox = 4
	terminalSessionLimitPerReplica = 64
)

// terminalTicketLimiter bounds ticket-signing and CubeMaster lookup work on a
// single CubeOps replica. Limits are deliberately per replica so terminal
// tickets remain stateless and work across an HA deployment.
type terminalTicketLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int
	window   time.Duration
}

func newTerminalTicketLimiter(limit int, window time.Duration) *terminalTicketLimiter {
	return &terminalTicketLimiter{
		attempts: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

func (l *terminalTicketLimiter) allow(user string) bool {
	return l.allowAt(user, time.Now())
}

func (l *terminalTicketLimiter) allowAt(user string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.window)
	for key, attempts := range l.attempts {
		kept := attempts[:0]
		for _, attempt := range attempts {
			if attempt.After(cutoff) {
				kept = append(kept, attempt)
			}
		}
		if len(kept) == 0 {
			delete(l.attempts, key)
			continue
		}
		l.attempts[key] = kept
	}

	if len(l.attempts[user]) >= l.limit {
		return false
	}
	l.attempts[user] = append(l.attempts[user], now)
	return true
}

// terminalSessionLimiter bounds live terminal sessions on one CubeOps replica.
// The returned release function is idempotent so every handler exit path can
// safely defer it.
type terminalSessionLimiter struct {
	mu            sync.Mutex
	global        int
	byUser        map[string]int
	bySandbox     map[string]int
	maxGlobal     int
	maxPerUser    int
	maxPerSandbox int
}

func newTerminalSessionLimiter(maxGlobal, maxPerUser, maxPerSandbox int) *terminalSessionLimiter {
	return &terminalSessionLimiter{
		byUser:        make(map[string]int),
		bySandbox:     make(map[string]int),
		maxGlobal:     maxGlobal,
		maxPerUser:    maxPerUser,
		maxPerSandbox: maxPerSandbox,
	}
}

func (l *terminalSessionLimiter) acquire(user, sandboxID string) (func(), bool) {
	l.mu.Lock()
	if l.global >= l.maxGlobal ||
		l.byUser[user] >= l.maxPerUser ||
		l.bySandbox[sandboxID] >= l.maxPerSandbox {
		l.mu.Unlock()
		return nil, false
	}
	l.global++
	l.byUser[user]++
	l.bySandbox[sandboxID]++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.global--
			decrementTerminalLimit(l.byUser, user)
			decrementTerminalLimit(l.bySandbox, sandboxID)
		})
	}, true
}

func decrementTerminalLimit(counts map[string]int, key string) {
	if counts[key] <= 1 {
		delete(counts, key)
		return
	}
	counts[key]--
}

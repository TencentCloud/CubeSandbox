// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package eventbus fans Redis Pub/Sub wakeup hints out to local waiters.
// It deliberately keeps no event history: Redis remains the source of truth,
// and callers read the current state after every wakeup.
package eventbus

import (
	"sync"
	"sync/atomic"
)

// Bus is safe for concurrent use.
type Bus struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

func New() *Bus {
	return &Bus{
		waiters: make(map[string]map[chan struct{}]struct{}),
	}
}

// Publish fans a wakeup hint out to current waiters. It never blocks and does
// not retain the event for future waiters.
func (b *Bus) Publish(sandboxID string) {
	if sandboxID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.waiters[sandboxID] {
		select {
		case ch <- struct{}{}:
		default:
			// A hint is already pending. Redis will be read when it is
			// consumed, so another hint adds no information.
		}
	}
}

// Wait registers a listener. The caller must invoke cancel when done.
func (b *Bus) Wait(sandboxID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	b.mu.Lock()
	set, ok := b.waiters[sandboxID]
	if !ok {
		set = make(map[chan struct{}]struct{}, 1)
		b.waiters[sandboxID] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()

	var cancelled atomic.Bool
	cancel := func() {
		if !cancelled.CompareAndSwap(false, true) {
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if set, ok := b.waiters[sandboxID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(b.waiters, sandboxID)
			}
		}
	}
	return ch, cancel
}

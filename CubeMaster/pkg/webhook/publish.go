// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"fmt"
	"sync"
)

// NopPublisher is the default until publication is explicitly wired.
type NopPublisher struct{}

func (NopPublisher) TryPublish(*CapturedEvent) PublishResult {
	return PublishResult{Status: PublishDisabled}
}

// BufferedPublisher must be constructed with NewBufferedPublisher and not copied.
type BufferedPublisher struct {
	mu      sync.RWMutex
	stopped bool
	events  chan *CapturedEvent
}

func NewBufferedPublisher(capacity int) (*BufferedPublisher, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("webhook: capacity must be positive")
	}
	return &BufferedPublisher{events: make(chan *CapturedEvent, capacity)}, nil
}

func (p *BufferedPublisher) TryPublish(ev *CapturedEvent) PublishResult {
	// Only Stop takes the write lock. A pending/active Stop rejects admission
	// immediately; publishers never wait behind shutdown or other publishers.
	if !p.mu.TryRLock() {
		return PublishResult{Status: PublishStopped}
	}
	defer p.mu.RUnlock()
	if p.stopped {
		return PublishResult{Status: PublishStopped}
	}
	if err := ev.validate(); err != nil {
		return PublishResult{Status: PublishInvalid, Err: err}
	}
	select {
	case p.events <- ev:
		return PublishResult{Status: PublishAccepted}
	default:
		return PublishResult{Status: PublishFull}
	}
}

// Events is the production consumer path. The channel is never closed:
// consumers must coordinate their own lifetime, not range until shutdown.
func (p *BufferedPublisher) Events() <-chan *CapturedEvent { return p.events }

// Stop waits only for admitted local publication sections, never consumption
// or persistence. Taking the write lock closes admission and joins all senders.
// Every concurrent Stop acquires it, so every return has the same guarantee:
// no later enqueue is possible, and accepted events remain readable.
func (p *BufferedPublisher) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}

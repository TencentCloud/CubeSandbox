// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"sync"
)

// Ingress admits complete batches into an event-counted in-memory queue.
type Ingress struct {
	mu       sync.Mutex
	queued   int
	capacity int
	closed   bool
	batches  chan InternalBatch
	stats    *Stats
}

// NewIngress creates an ingress queue whose capacity is measured in events.
func NewIngress(capacity int) *Ingress {
	return newIngress(capacity, NewStats())
}

func newIngress(capacity int, stats *Stats) *Ingress {
	return &Ingress{
		capacity: capacity,
		batches:  make(chan InternalBatch, capacity),
		stats:    stats,
	}
}

// TryEnqueue admits all events in a batch or rejects the whole batch.
func (q *Ingress) TryEnqueue(batch InternalBatch) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	count := len(batch.Events)
	if q.closed || count == 0 || count > q.capacity-q.queued {
		q.stats.ingressRejectedBatches.Add(1)
		return false
	}
	select {
	case q.batches <- batch:
		q.queued += count
		q.stats.ingressAcceptedBatches.Add(1)
		q.stats.ingressAcceptedEvents.Add(uint64(count))
		return true
	default:
		return false
	}
}

// Stats returns the ingress statistics registry.
func (q *Ingress) Stats() *Stats {
	return q.stats
}

// Queued reports the current number of queued events.
func (q *Ingress) Queued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queued
}

// Close stops admission of new batches. Already queued batches remain readable.
func (q *Ingress) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
}

// Receive waits for the next complete batch.
func (q *Ingress) Receive(ctx context.Context) (InternalBatch, bool) {
	select {
	case <-ctx.Done():
		return InternalBatch{}, false
	case batch, ok := <-q.batches:
		if !ok {
			return InternalBatch{}, false
		}
		q.mu.Lock()
		q.queued -= len(batch.Events)
		q.mu.Unlock()
		return batch, true
	}
}

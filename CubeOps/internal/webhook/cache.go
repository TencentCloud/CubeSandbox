// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"sort"
	"sync"
	"time"
)

// topNSoftLimitCap bounds the over-limit subscription ids fed into the NOT IN
// clause (unbounded parameter lists would exceed SQL limits).
const topNSoftLimitCap = 100

// BacklogCache is the shared, periodically-refreshed actionable-backlog view
// used by both the consumer (global watermark backpressure) and the delivery
// supervisor (per-subscription soft limit). Both use the same window
// predicate, so counts and claim guards agree.
type BacklogCache struct {
	mu     sync.RWMutex
	global map[string]int64
	perSub map[int64]int64
	window time.Duration
}

// NewBacklogCache creates a cache over the given keep-pending window.
func NewBacklogCache(window time.Duration) *BacklogCache {
	return &BacklogCache{
		global: map[string]int64{},
		perSub: map[int64]int64{},
		window: window,
	}
}

// Refresh recomputes global + per-subscription backlog counts from SQL and
// publishes the global split to the backlog-by-status gauge.
func (c *BacklogCache) Refresh(ctx context.Context, store *DeliveryStore) error {
	global, err := store.BacklogCounts(ctx, c.window)
	if err != nil {
		return err
	}
	perSub, err := store.SubscriptionBacklogs(ctx, c.window)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.global = global
	c.perSub = perSub
	c.mu.Unlock()
	for _, st := range []string{StatusPending, StatusFailed} {
		backlogByStatus.WithLabelValues(st).Set(0)
	}
	for st, n := range global {
		backlogByStatus.WithLabelValues(st).Set(float64(n))
	}
	return nil
}

// Global returns the total actionable backlog (pending + retryable failed).
func (c *BacklogCache) Global() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var n int64
	for _, v := range c.global {
		n += v
	}
	return n
}

// OverLimit returns the Top-N subscription ids whose actionable backlog is at
// or above limit. Empty when none qualify.
func (c *BacklogCache) OverLimit(limit int) []int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	type kv struct {
		id int64
		n  int64
	}
	var all []kv
	for id, n := range c.perSub {
		if n >= int64(limit) {
			all = append(all, kv{id: id, n: n})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].id < all[j].id
	})
	if len(all) > topNSoftLimitCap {
		all = all[:topNSoftLimitCap]
	}
	out := make([]int64, 0, len(all))
	for _, k := range all {
		out = append(out, k.id)
	}
	return out
}

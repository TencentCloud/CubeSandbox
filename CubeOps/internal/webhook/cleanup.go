// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
)

// Cleanup periodically removes terminal rows past their retention windows and
// sweeps keep-pending rows past the retry window to dead. No distributed lock:
// concurrent batched DELETEs are idempotent and batch sizes bound tx length.
type Cleanup struct {
	store              *DeliveryStore
	succeededRetention time.Duration
	terminalRetention  time.Duration
	keepPendingWindow  time.Duration
	interval           time.Duration
}

// NewCleanup builds the retention/sweep loop.
func NewCleanup(
	store *DeliveryStore,
	succeededRetention, terminalRetention, keepPendingWindow, interval time.Duration,
) *Cleanup {
	return &Cleanup{
		store:              store,
		succeededRetention: succeededRetention,
		terminalRetention:  terminalRetention,
		keepPendingWindow:  keepPendingWindow,
		interval:           interval,
	}
}

// Run executes cleanup on every interval until ctx is canceled.
func (c *Cleanup) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	c.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

func (c *Cleanup) runOnce(ctx context.Context) {
	deleted, err := c.store.RetentionCleanup(ctx, c.succeededRetention, c.terminalRetention, 500)
	if err != nil {
		logging.G(ctx).Errorf("webhook cleanup: retention: %v", err)
	}
	if deleted > 0 {
		logging.G(ctx).Infof("webhook cleanup: removed %d terminal rows", deleted)
	}
	swept, err := c.store.SweepKeepPendingWindow(ctx, c.keepPendingWindow)
	if err != nil {
		logging.G(ctx).Errorf("webhook cleanup: keep-pending sweep: %v", err)
	}
	if swept > 0 {
		logging.G(ctx).Infof("webhook cleanup: keep-pending window converted %d rows to dead", swept)
	}
}

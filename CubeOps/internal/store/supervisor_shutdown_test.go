// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/webhook"
)

// delaySender succeeds after a fixed delay, or reports shutdown when its
// context is cancelled first.
type delaySender struct{ delay time.Duration }

func (s delaySender) Send(ctx context.Context, _ *webhook.DeliveryForSend) webhook.SendResult {
	select {
	case <-time.After(s.delay):
		return webhook.SendResult{Class: webhook.ResultSucceeded, HTTPStatus: 200}
	case <-ctx.Done():
		return webhook.SendResult{Class: webhook.ResultShutdown, Err: ctx.Err()}
	}
}

// TestSupervisor_GracefulShutdownRecordsInFlightSend guards the shutdown
// accounting path: a send that finishes INSIDE the grace window must be
// recorded as succeeded. Regression test for the bug where sendOne wrote the
// completion with the claim loop's context, which Shutdown cancels in step ①
// before the grace window — the UPDATE then failed with "context canceled",
// the row stayed in_progress and was redelivered after lease expiry on every
// restart.
func TestSupervisor_GracefulShutdownRecordsInFlightSend(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	ctx := context.Background()

	sub := newWebhookSub("shutdown-grace", "https://example.com/hook", "sandbox.created")
	if err := env.store.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}
	// No next_retry_at backdating: materialized rows are due immediately.
	if _, err := ds.MaterializeDeliveries(ctx, "evt:shutdown", []byte(`{"event":"sandbox.created"}`), []int64{sub.ID}, 10); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	sup := webhook.NewSupervisor(
		ds, delaySender{300 * time.Millisecond}, webhook.NewBacklogCache(0),
		"owner-shutdown",
		30*time.Second,      // lease
		0,                   // keep-pending window
		50*time.Millisecond, // poll
		2*time.Second,       // shutdownTimeout (grace) — longer than the send
		10,                  // claimBatch
		4,                   // workerConcurrency
		2,                   // perSubConcurrency
		1000,                // softLimit
		5,                   // maxAttempts
		"keep-pending",
	)
	sup.Start(ctx)

	waitFor(t, 10*time.Second, func() bool {
		rows, err := env.store.ListWebhookDeliveries(ctx, sub.ID, "in_progress", "", 0, 0)
		return err == nil && len(rows) == 1
	})

	sup.Shutdown(ctx)

	rows, err := env.store.ListWebhookDeliveries(ctx, sub.ID, "", "", 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v rows=%d", err, len(rows))
	}
	if rows[0].Status != webhook.StatusSucceeded {
		t.Fatalf("send finished inside grace window but status=%q attempts=%d (want succeeded)",
			rows[0].Status, rows[0].Attempts)
	}
	if rows[0].Attempts != 0 {
		t.Fatalf("successful send must not increment attempts, got %d", rows[0].Attempts)
	}
}

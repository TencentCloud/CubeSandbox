// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/webhook"
)

const (
	workerTestStreamKey = "cube:v1:shared:sandbox:lifecycle:events"
)

func newWorkerRedis(t *testing.T) (redis.UniversalClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// TestConsumer_MaterializesAndAcks drives the full consumer loop against
// miniredis (stream + consumer group) and a real MySQL ledger: XADD a create
// event → the consumer materializes a pending delivery row and ACKs it.
func TestConsumer_MaterializesAndAcks(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	rdb, _ := newWorkerRedis(t)
	ctx := context.Background()

	sub := newWebhookSub("consumer-test", "https://example.com/hook", "sandbox.created")
	if err := env.store.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}

	backlog := webhook.NewBacklogCache(0)
	consumer := webhook.NewConsumer(rdb, ds, env.store, "cube-webhook", "test-1",
		10, 100*time.Millisecond, 0, time.Second, 10000, 200, backlog)
	// Create the group BEFORE publishing: the consumer creates it at "$" on
	// startup, and an entry published before group creation would sit behind
	// the group cursor and never be delivered (real-world startup window, but
	// the test must be deterministic).
	if err := rdb.XGroupCreateMkStream(ctx, workerTestStreamKey, "cube-webhook", "$").Err(); err != nil {
		t.Fatalf("pre-create group: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumer.Run(runCtx)

	_, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: workerTestStreamKey,
		ID:     "*",
		Values: map[string]interface{}{
			"op": "create", "sandbox_id": "sbx-consumer",
			"ts":      time.Now().UnixMilli(),
			"payload": `{"sandbox_id":"sbx-consumer","template_id":"tpl-consumer"}`,
		},
	}).Result()
	if err != nil {
		t.Fatalf("xadd: %v", err)
	}

	waitFor(t, 15*time.Second, func() bool {
		rows, err := env.store.ListWebhookDeliveries(ctx, sub.ID, "", "", 0, 0)
		return err == nil && len(rows) == 1
	})
	rows, _ := env.store.ListWebhookDeliveries(ctx, sub.ID, "", "", 0, 0)
	if rows[0].Status != webhook.StatusPending {
		t.Fatalf("status = %q, want pending", rows[0].Status)
	}
	var pp map[string]interface{}
	if err := json.Unmarshal([]byte(rows[0].Payload), &pp); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if pp["event"] != "sandbox.created" || pp["template_id"] != "tpl-consumer" || pp["sandbox_id"] != "sbx-consumer" {
		t.Fatalf("payload mismatch: %v", pp)
	}

	// The entry must be ACKed (no pending entries left for the group).
	waitFor(t, 10*time.Second, func() bool {
		pending, err := rdb.XPending(ctx, workerTestStreamKey, "cube-webhook").Result()
		return err == nil && pending.Count == 0
	})
}

// TestConsumer_NoSubscribersAcks ensures events nobody subscribed to are
// ACKed without creating rows.
func TestConsumer_NoSubscribersAcks(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	rdb, _ := newWorkerRedis(t)
	ctx := context.Background()

	backlog := webhook.NewBacklogCache(0)
	consumer := webhook.NewConsumer(rdb, ds, env.store, "cube-webhook", "test-1",
		10, 100*time.Millisecond, 0, time.Second, 10000, 200, backlog)
	// See TestConsumer_MaterializesAndAcks: pre-create the group so the entry
	// is delivered deterministically.
	if err := rdb.XGroupCreateMkStream(ctx, workerTestStreamKey, "cube-webhook", "$").Err(); err != nil {
		t.Fatalf("pre-create group: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumer.Run(runCtx)

	_, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: workerTestStreamKey,
		ID:     "*",
		Values: map[string]interface{}{
			"op": "state", "sandbox_id": "sbx-none",
			"ts":      time.Now().UnixMilli(),
			"payload": `{"state":"paused","source":"api"}`,
		},
	}).Result()
	if err != nil {
		t.Fatalf("xadd: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		pending, err := rdb.XPending(ctx, workerTestStreamKey, "cube-webhook").Result()
		return err == nil && pending.Count == 0
	})
}

// fakeSender records sends without binding a listener.
type fakeSender struct {
	mu      sync.Mutex
	results map[int64]webhook.SendResult
	got     []webhook.DeliveryForSend
}

func newFakeSender(results map[int64]webhook.SendResult) *fakeSender {
	return &fakeSender{results: results}
}

func (f *fakeSender) Send(_ context.Context, d *webhook.DeliveryForSend) webhook.SendResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, *d)
	if r, ok := f.results[d.ID]; ok {
		return r
	}
	return webhook.SendResult{Class: webhook.ResultSucceeded, HTTPStatus: 200}
}

func (f *fakeSender) sends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func seedDueDelivery(t *testing.T, env *testStoreEnv, ds *webhook.DeliveryStore, eventID string, subID int64) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := ds.MaterializeDeliveries(ctx, eventID, []byte(`{"event":"sandbox.created"}`), []int64{subID}, 10); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Backdate so the claim query (server now()) picks it up immediately.
	if err := env.store.DB().Exec(
		`UPDATE t_webhook_delivery SET next_retry_at = now() - INTERVAL 1 SECOND WHERE event_id = ?`, eventID).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}
	var id int64
	if err := env.store.DB().Raw(
		`SELECT id FROM t_webhook_delivery WHERE event_id = ?`, eventID).Scan(&id).Error; err != nil {
		t.Fatalf("load id: %v", err)
	}
	return id
}

// TestSupervisor_SendsAndCompletes drives the claim loop with a fake sender
// against a real MySQL ledger: pending → claimed → sent → succeeded.
func TestSupervisor_SendsAndCompletes(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	ctx := context.Background()

	sub := newWebhookSub("supervisor-test", "https://example.com/hook", "sandbox.created")
	if err := env.store.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}
	deliveryID := seedDueDelivery(t, env, ds, "evt:super", sub.ID)

	backlog := webhook.NewBacklogCache(0)
	if err := backlog.Refresh(ctx, ds); err != nil {
		t.Fatalf("backlog refresh: %v", err)
	}
	fake := newFakeSender(map[int64]webhook.SendResult{})
	sup := webhook.NewSupervisor(ds, fake, backlog, "sup-1",
		60*time.Second, 0, 50*time.Millisecond, 5*time.Second,
		8, 4, 2, 1000, 5, "keep-pending")
	runCtx, cancel := context.WithCancel(context.Background())
	sup.Start(runCtx)
	defer cancel()

	waitFor(t, 15*time.Second, func() bool {
		rows, err := env.store.ListWebhookDeliveries(ctx, sub.ID, webhook.StatusSucceeded, "", 0, 0)
		return err == nil && len(rows) == 1
	})
	if fake.sends() != 1 {
		t.Fatalf("sender calls = %d, want 1", fake.sends())
	}
	_ = deliveryID
}

// TestSupervisor_RetryableIncrementsAttempts verifies failed sends land in
// failed with attempts=1 and the lease is released.
func TestSupervisor_RetryableIncrementsAttempts(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	ctx := context.Background()

	sub := newWebhookSub("supervisor-retry", "https://example.com/hook", "sandbox.created")
	if err := env.store.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}
	deliveryID := seedDueDelivery(t, env, ds, "evt:retry", sub.ID)

	backlog := webhook.NewBacklogCache(0)
	_ = backlog.Refresh(ctx, ds)
	fake := newFakeSender(map[int64]webhook.SendResult{
		deliveryID: {Class: webhook.ResultRetryable, HTTPStatus: 500},
	})
	sup := webhook.NewSupervisor(ds, fake, backlog, "sup-2",
		60*time.Second, 0, 50*time.Millisecond, 5*time.Second,
		8, 4, 2, 1000, 5, "keep-pending")
	runCtx, cancel := context.WithCancel(context.Background())
	sup.Start(runCtx)
	defer cancel()

	waitFor(t, 15*time.Second, func() bool {
		rows, err := env.store.ListWebhookDeliveries(ctx, sub.ID, webhook.StatusFailed, "", 0, 0)
		return err == nil && len(rows) == 1
	})
	rows, _ := env.store.ListWebhookDeliveries(ctx, sub.ID, webhook.StatusFailed, "", 0, 0)
	if rows[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", rows[0].Attempts)
	}
	if rows[0].LeaseOwner != nil {
		t.Fatalf("lease owner should be cleared after completion")
	}
}

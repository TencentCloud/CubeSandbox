// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/webhook"
)

// newDeliveryStore wires the webhook delivery store over the test DB.
func newDeliveryStore(t *testing.T) (*testStoreEnv, *webhook.DeliveryStore) {
	t.Helper()
	env := newTestStore(t)
	return env, webhook.NewDeliveryStore(env.store.DB())
}

func TestDelivery_MaterializeIdempotentAndChunked(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	ctx := context.Background()

	sub := newWebhookSub("mat", "https://example.com/hook", "sandbox.created")
	if err := env.store.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}

	n, err := ds.MaterializeDeliveries(ctx, "evt:1", []byte(`{"event":"sandbox.created"}`), []int64{sub.ID}, 1)
	if err != nil || n != 1 {
		t.Fatalf("first materialize: n=%d err=%v, want 1", n, err)
	}
	// Idempotent: same event × subscription inserts nothing.
	n, err = ds.MaterializeDeliveries(ctx, "evt:1", []byte(`{"event":"sandbox.created"}`), []int64{sub.ID}, 1)
	if err != nil || n != 0 {
		t.Fatalf("duplicate materialize: n=%d err=%v, want 0", n, err)
	}
	// Chunked fan-out: 3 subscriptions, chunk 2 → 2 transactions, 3 rows.
	ids := []int64{}
	for i := 0; i < 3; i++ {
		s := newWebhookSub(fmt.Sprintf("mat-%d", i), "https://example.com/hook", "sandbox.created")
		if err := env.store.CreateWebhookSubscription(ctx, s); err != nil {
			t.Fatalf("create sub %d: %v", i, err)
		}
		ids = append(ids, s.ID)
	}
	n, err = ds.MaterializeDeliveries(ctx, "evt:2", []byte(`{}`), ids, 2)
	if err != nil || n != 3 {
		t.Fatalf("chunked materialize: n=%d err=%v, want 3", n, err)
	}
}

func TestDelivery_ClaimCompleteLifecycle(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	ctx := context.Background()

	secret := "hook-secret"
	enc, err := crypto.EncryptSecret(secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	sub := &store.WebhookSubscription{
		Name: "lifecycle", URL: "https://example.com/hook", Enabled: true,
		SecretCiphertext: &enc,
		Events:           []store.WebhookSubscriptionEvent{{EventType: "sandbox.created"}},
	}
	if err := env.store.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}
	if _, err := ds.MaterializeDeliveries(ctx, "evt:l", []byte(`{"event":"sandbox.created"}`), []int64{sub.ID}, 10); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Materialization stamps next_retry_at with the client clock, which can be
	// a few microseconds ahead of the server now() used by the claim query
	// (self-heals within one poll in production). Backdate for determinism.
	if err := env.store.DB().Exec(
		`UPDATE t_webhook_delivery SET next_retry_at = now() - INTERVAL 1 SECOND WHERE event_id = ?`, "evt:l").Error; err != nil {
		t.Fatalf("backdate next_retry_at: %v", err)
	}

	ids, err := ds.ClaimCandidates(ctx, webhook.ClaimQuery{Limit: 10})
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(ids))
	}

	// First claim wins; a second worker loses.
	ok, err := ds.Claim(ctx, ids[0], "worker-a", 60*time.Second, 0)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	ok, err = ds.Claim(ctx, ids[0], "worker-b", 60*time.Second, 0)
	if err != nil || ok {
		t.Fatalf("second claim should lose: ok=%v err=%v", ok, err)
	}

	// Load for send: url + decrypted secret.
	d, err := ds.LoadDeliveryForSend(ctx, ids[0])
	if err != nil {
		t.Fatalf("load for send: %v", err)
	}
	if d.URL != "https://example.com/hook" || d.Secret != secret {
		t.Fatalf("loaded url=%q secret=%q", d.URL, d.Secret)
	}

	// Successful completion clears the lease.
	ok, err = ds.Complete(ctx, ids[0], "worker-a", webhook.Completion{
		Result: webhook.ResultSucceeded, HTTPStatus: intPtr(200),
	})
	if err != nil || !ok {
		t.Fatalf("complete: ok=%v err=%v", ok, err)
	}
	// Late completion from the same owner is dropped (lease already cleared).
	ok, err = ds.Complete(ctx, ids[0], "worker-a", webhook.Completion{
		Result: webhook.ResultSucceeded, HTTPStatus: intPtr(200),
	})
	if err != nil || ok {
		t.Fatalf("late complete should drop: ok=%v err=%v", ok, err)
	}
}

func TestDelivery_RetryableBacklogAndKeepPendingSweep(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	ctx := context.Background()

	sub := newWebhookSub("retry", "https://example.com/hook", "sandbox.created")
	if err := env.store.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}
	if _, err := ds.MaterializeDeliveries(ctx, "evt:r", []byte(`{}`), []int64{sub.ID}, 10); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if err := env.store.DB().Exec(
		`UPDATE t_webhook_delivery SET next_retry_at = now() - INTERVAL 1 SECOND WHERE event_id = ?`, "evt:r").Error; err != nil {
		t.Fatalf("backdate next_retry_at: %v", err)
	}
	ids, err := ds.ClaimCandidates(ctx, webhook.ClaimQuery{Limit: 10})
	if err != nil || len(ids) != 1 {
		t.Fatalf("candidates: ids=%v err=%v", ids, err)
	}
	if _, err := ds.Claim(ctx, ids[0], "w", 60*time.Second, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}
	later := time.Now().Add(time.Minute)
	lastErr := "boom"
	ok, err := ds.Complete(ctx, ids[0], "w", webhook.Completion{
		Result: webhook.ResultRetryable, HTTPStatus: intPtr(500),
		LastError: &lastErr, NextRetryAt: later,
	})
	if err != nil || !ok {
		t.Fatalf("retryable complete: ok=%v err=%v", ok, err)
	}

	counts, err := ds.BacklogCounts(ctx, 0)
	if err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if counts[webhook.StatusFailed] != 1 {
		t.Fatalf("backlog failed = %d, want 1", counts[webhook.StatusFailed])
	}

	// Age the row beyond a 1-hour window and sweep → dead.
	if err := env.store.DB().Exec(
		`UPDATE t_webhook_delivery SET first_failed_at = now() - INTERVAL 2 HOUR WHERE id = ?`, ids[0]).Error; err != nil {
		t.Fatalf("age row: %v", err)
	}
	swept, err := ds.SweepKeepPendingWindow(ctx, time.Hour)
	if err != nil || swept != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", swept, err)
	}
	counts, err = ds.BacklogCounts(ctx, time.Hour)
	if err != nil {
		t.Fatalf("backlog after sweep: %v", err)
	}
	if counts[webhook.StatusFailed] != 0 {
		t.Fatalf("dead rows must not count as backlog: %+v", counts)
	}
}

func TestDelivery_MaterializationFailureUpsert(t *testing.T) {
	env, ds := newDeliveryStore(t)
	defer env.teardown()
	ctx := context.Background()

	n1, err := ds.RecordMaterializationFailure(ctx, "poison:1", "sbx-1", nil, "create", []byte(`{}`), "bad payload")
	if err != nil || n1 != 1 {
		t.Fatalf("first failure: n=%d err=%v, want 1", n1, err)
	}
	n2, err := ds.RecordMaterializationFailure(ctx, "poison:1", "sbx-1", nil, "create", []byte(`{}`), "bad payload again")
	if err != nil || n2 != 2 {
		t.Fatalf("second failure: n=%d err=%v, want 2", n2, err)
	}
	got, err := ds.MaterializationFailureAttempts(ctx, "poison:1")
	if err != nil || got != 2 {
		t.Fatalf("attempts: got=%d err=%v, want 2", got, err)
	}
}

func intPtr(v int) *int { return &v }

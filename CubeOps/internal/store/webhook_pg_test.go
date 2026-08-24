// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"

	"github.com/tencentcloud/CubeSandbox/CubeDB/dao"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/webhook"
)

// newTestStorePG mirrors newTestStore but against a throwaway PostgreSQL
// container, so the dialect-specific delivery SQL (ON CONFLICT, interval,
// RETURNING, subquery DELETE) is exercised for real.
func newTestStorePG(t *testing.T) *testStoreEnv {
	t.Helper()
	pool, err := dockertest.NewPool("")
	if err != nil {
		abortOrSkipDocker(t, "dockertest not available (%v)", err)
	}
	if err := pool.Client.Ping(); err != nil {
		abortOrSkipDocker(t, "docker daemon not reachable (%v)", err)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "16-alpine",
		Env: []string{
			"POSTGRES_PASSWORD=postgres",
			"POSTGRES_DB=cubeops_test",
		},
	}, func(hostConfig *docker.HostConfig) {
		hostConfig.AutoRemove = true
		hostConfig.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		abortOrSkipDocker(t, "could not start postgres container (%v)", err)
	}
	port := resource.GetPort("5432/tcp")
	dsn := fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%s/cubeops_test?sslmode=disable", port)

	pool.MaxWait = containerProbeTimeout
	if err := pool.Retry(func() error {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("postgres container never became reachable: %v", err)
	}

	cfg := dao.Config{
		Driver:       "postgres",
		User:         "postgres",
		Pwd:          "postgres",
		Addr:         fmt.Sprintf("127.0.0.1:%s", port),
		DBName:       "cubeops_test",
		MaxIdleConns: 5,
		MaxOpenConns: 10,
	}
	crypto.ResetMasterKeyForTest()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := store.New(ctx, cfg)
	if err != nil {
		_ = pool.Purge(resource)
		t.Fatalf("store.New(postgres): %v", err)
	}
	return &testStoreEnv{
		store: s,
		dsn:   dsn,
		teardown: func() {
			_ = s.Close()
			_ = pool.Purge(resource)
		},
	}
}

// TestDelivery_Postgres exercises the dialect-specific delivery SQL against
// a real PostgreSQL: materialization idempotency (ON CONFLICT), claim with
// the keep-pending interval guard, completion, lease release, keep-pending
// sweep, retention cleanup (subquery DELETE) and materialization failure
// upsert (RETURNING attempts).
func TestDelivery_Postgres(t *testing.T) {
	env := newTestStorePG(t)
	defer env.teardown()
	ds := webhook.NewDeliveryStore(env.store.DB())
	ctx := context.Background()

	sub := newWebhookSub("pg-delivery", "https://example.com/hook", "sandbox.created")
	if err := env.store.CreateWebhookSubscription(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}

	// 1. Materialize + idempotency (ON CONFLICT DO NOTHING).
	n, err := ds.MaterializeDeliveries(ctx, "pg:1", []byte(`{"event":"sandbox.created"}`), []int64{sub.ID}, 10)
	if err != nil || n != 1 {
		t.Fatalf("materialize: n=%d err=%v", n, err)
	}
	n, err = ds.MaterializeDeliveries(ctx, "pg:1", []byte(`{"event":"sandbox.created"}`), []int64{sub.ID}, 10)
	if err != nil || n != 0 {
		t.Fatalf("duplicate materialize: n=%d err=%v, want 0", n, err)
	}

	// 2. Backdate (PG syntax) then claim with an active keep-pending window
	// so the interval guard is exercised on both queries.
	if err := env.store.DB().Exec(
		`UPDATE t_webhook_delivery SET next_retry_at = now() - interval '1 second' WHERE event_id = 'pg:1'`).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}
	ids, err := ds.ClaimCandidatesDue(ctx, webhook.ClaimQuery{Limit: 10, KeepPendingWindow: time.Hour})
	if err != nil || len(ids) != 1 {
		t.Fatalf("due candidates: ids=%v err=%v", ids, err)
	}
	ok, err := ds.Claim(ctx, ids[0], "pg-worker", 60*time.Second, time.Hour)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if ok2, _ := ds.Claim(ctx, ids[0], "pg-worker-2", 60*time.Second, time.Hour); ok2 {
		t.Fatal("second claim must lose the lease")
	}

	// 3. Load + succeed (clears lease), then a late completion is dropped.
	d, err := ds.LoadDeliveryForSend(ctx, ids[0])
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d.URL != "https://example.com/hook" {
		t.Fatalf("url = %q", d.URL)
	}
	status := 200
	ok, err = ds.Complete(ctx, ids[0], "pg-worker", webhook.Completion{Result: webhook.ResultSucceeded, HTTPStatus: &status})
	if err != nil || !ok {
		t.Fatalf("complete: ok=%v err=%v", ok, err)
	}
	if ok, _ := ds.Complete(ctx, ids[0], "pg-worker", webhook.Completion{Result: webhook.ResultSucceeded, HTTPStatus: &status}); ok {
		t.Fatal("late completion must be dropped")
	}

	// 4. Retryable completion: attempts increments atomically, first_failed_at
	// is set via COALESCE.
	if _, err := ds.MaterializeDeliveries(ctx, "pg:2", []byte(`{}`), []int64{sub.ID}, 10); err != nil {
		t.Fatalf("materialize pg:2: %v", err)
	}
	if err := env.store.DB().Exec(
		`UPDATE t_webhook_delivery SET next_retry_at = now() - interval '1 second' WHERE event_id = 'pg:2'`).Error; err != nil {
		t.Fatalf("backdate pg:2: %v", err)
	}
	ids2, err := ds.ClaimCandidatesDue(ctx, webhook.ClaimQuery{Limit: 10, KeepPendingWindow: time.Hour})
	if err != nil || len(ids2) != 1 {
		t.Fatalf("due candidates pg:2: ids=%v err=%v", ids2, err)
	}
	if ok, _ := ds.Claim(ctx, ids2[0], "pg-worker", 60*time.Second, time.Hour); !ok {
		t.Fatal("claim pg:2 failed")
	}
	msg := "boom"
	if ok, _ := ds.Complete(ctx, ids2[0], "pg-worker", webhook.Completion{
		Result: webhook.ResultRetryable, HTTPStatus: &status, LastError: &msg, NextRetryDelay: time.Minute,
	}); !ok {
		t.Fatal("retryable complete failed")
	}
	var attempts int
	if err := env.store.DB().Raw(
		`SELECT attempts FROM t_webhook_delivery WHERE id = ?`, ids2[0]).Scan(&attempts).Error; err != nil || attempts != 1 {
		t.Fatalf("attempts = %d err=%v, want 1", attempts, err)
	}

	// 5. Keep-pending sweep: age the failed row and convert to dead.
	if err := env.store.DB().Exec(
		`UPDATE t_webhook_delivery SET first_failed_at = now() - interval '2 hour' WHERE id = ?`, ids2[0]).Error; err != nil {
		t.Fatalf("age row: %v", err)
	}
	swept, err := ds.SweepKeepPendingWindow(ctx, time.Hour)
	if err != nil || swept != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", swept, err)
	}

	// 6. Retention cleanup (subquery DELETE): old succeeded removed, old
	// failed (retryable) untouched.
	if err := env.store.DB().Exec(
		`INSERT INTO t_webhook_delivery
			(event_id, subscription_id, payload, status, attempts, next_retry_at, updated_at)
		 VALUES ('pg:old-succeeded', ?, '{}', 'succeeded', 0, now(), now() - interval '40 day')`, sub.ID).Error; err != nil {
		t.Fatalf("insert old succeeded: %v", err)
	}
	cleaned, err := ds.RetentionCleanup(ctx, 30*24*time.Hour, 90*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("retention cleaned %d, want 1 (old succeeded only; failed rows untouched)", cleaned)
	}

	// 7. Materialization failure upsert with RETURNING attempts.
	a1, err := ds.RecordMaterializationFailure(ctx, "pg:poison", "sbx-1", nil, "create", []byte(`{}`), "bad")
	if err != nil || a1 != 1 {
		t.Fatalf("failure 1: a=%d err=%v", a1, err)
	}
	a2, err := ds.RecordMaterializationFailure(ctx, "pg:poison", "sbx-1", nil, "create", []byte(`{}`), "bad again")
	if err != nil || a2 != 2 {
		t.Fatalf("failure 2: a=%d err=%v", a2, err)
	}
}

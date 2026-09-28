// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redisstream

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

func newTestClient(t *testing.T) (*Client, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, zap.NewNop()), rdb
}

func TestRedisTransactionsProtectResumeOwnership(t *testing.T) {
	client, rdb := newTestClient(t)
	ctx := context.Background()

	if err := client.SetState(ctx, "sbx", lifecycle.StatePaused, time.Minute); err != nil {
		t.Fatal(err)
	}
	state, acquired, err := client.AcquireResume(ctx, "sbx", 10*time.Second)
	if err != nil || !acquired || state != lifecycle.StatePaused {
		t.Fatalf("AcquireResume() = (%q, %v, %v)", state, acquired, err)
	}

	state, acquired, err = client.AcquireResume(ctx, "sbx", 10*time.Second)
	if err != nil || acquired || state != "resuming" {
		t.Fatalf("second AcquireResume() = (%q, %v, %v)", state, acquired, err)
	}

	updated, err := client.WriteStateCAS(
		ctx, "sbx", lifecycle.StatePaused, lifecycle.StateRunning, time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("stale state event overwrote resuming ownership")
	}
	if got := rdb.Get(ctx, lifecycle.StateKey("sbx")).Val(); got != "resuming" {
		t.Fatalf("state = %q, want resuming", got)
	}

	updated, err = client.WriteStateCAS(
		ctx, "sbx", "resuming", lifecycle.StateRunning, time.Minute,
	)
	if err != nil || !updated {
		t.Fatalf("matching WriteStateCAS() = (%v, %v)", updated, err)
	}
	if got, _, err := client.GetState(ctx, "sbx"); err != nil || got != lifecycle.StateRunning {
		t.Fatalf("state = %q, want running", got)
	}
}

func TestRedisTransactionsProtectKillOwnership(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()

	if err := client.SetState(ctx, "sbx", lifecycle.StatePaused, time.Minute); err != nil {
		t.Fatal(err)
	}
	state, acquired, err := client.AcquireKill(ctx, "sbx", 10*time.Second)
	if err != nil || !acquired || state != lifecycle.StatePaused {
		t.Fatalf("AcquireKill() = (%q, %v, %v)", state, acquired, err)
	}

	state, acquired, err = client.AcquireKill(ctx, "sbx", 10*time.Second)
	if err != nil || acquired || state != "killing" {
		t.Fatalf("second AcquireKill() = (%q, %v, %v)", state, acquired, err)
	}

	state, acquired, err = client.AcquireResume(ctx, "sbx", 10*time.Second)
	if err != nil || acquired || state != "killing" {
		t.Fatalf("AcquireResume must not overwrite killing: (%q, %v, %v)", state, acquired, err)
	}
}

func TestAcquireKillAndResumeAreMutuallyExclusive(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()

	if err := client.SetState(ctx, "sbx-resume-first", lifecycle.StatePaused, time.Minute); err != nil {
		t.Fatal(err)
	}
	state, acquired, err := client.AcquireResume(ctx, "sbx-resume-first", 10*time.Second)
	if err != nil || !acquired || state != lifecycle.StatePaused {
		t.Fatalf("AcquireResume() = (%q, %v, %v)", state, acquired, err)
	}
	state, acquired, err = client.AcquireKill(ctx, "sbx-resume-first", 10*time.Second)
	if err != nil || acquired || state != "resuming" {
		t.Fatalf("AcquireKill must not overwrite resuming: (%q, %v, %v)", state, acquired, err)
	}

	state, acquired, err = client.AcquireKill(ctx, "sbx-empty", 10*time.Second)
	if err != nil || !acquired || state != "" {
		t.Fatalf("AcquireKill(empty) = (%q, %v, %v)", state, acquired, err)
	}
}

func TestGetStatesReturnsPresentKeys(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()

	if err := client.SetState(ctx, "a", lifecycle.StatePaused, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := client.SetState(ctx, "b", lifecycle.StateRunning, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetStates(ctx, []string{"a", "b", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != lifecycle.StatePaused || got["b"] != lifecycle.StateRunning {
		t.Fatalf("GetStates() = %v", got)
	}
	if _, ok := got["missing"]; ok {
		t.Fatal("missing key must be omitted")
	}
}

func TestCursorValidDetectsTrimmedHistory(t *testing.T) {
	client, rdb := newTestClient(t)
	ctx := context.Background()

	if err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: lifecycle.EventStreamKey,
		ID:     "100-0",
		Values: map[string]interface{}{"op": "create"},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	if valid, err := client.CursorValid(ctx, "99-0"); err != nil || valid {
		t.Fatalf("CursorValid(trimmed) = (%v, %v), want (false, nil)", valid, err)
	}
	if valid, err := client.CursorValid(ctx, "100-0"); err != nil || !valid {
		t.Fatalf("CursorValid(retained) = (%v, %v), want (true, nil)", valid, err)
	}
}

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

func TestRedisTransactionsProtectResumeOwnership(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	client := New(rdb, zap.NewNop())
	ctx := context.Background()

	if err := client.SetState(ctx, "sbx", lifecycle.StatePaused, time.Minute); err != nil {
		t.Fatal(err)
	}
	state, acquired, err := client.AcquireResume(ctx, "sbx", 10*time.Second)
	if err != nil || !acquired || state != "resuming" {
		t.Fatalf("AcquireResume() = (%q, %v, %v)", state, acquired, err)
	}

	state, acquired, err = client.AcquireResume(ctx, "sbx", 10*time.Second)
	if err != nil || acquired || state != "resuming" {
		t.Fatalf("second AcquireResume() = (%q, %v, %v)", state, acquired, err)
	}

	updated, err := client.WriteStateCAS(
		ctx, "sbx", lifecycle.StatePaused, lifecycle.StateRunning, "9-0", time.Minute,
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
		ctx, "sbx", "resuming", lifecycle.StateRunning, "10-0", time.Minute,
	)
	if err != nil || !updated {
		t.Fatalf("matching WriteStateCAS() = (%v, %v)", updated, err)
	}
	if got, _, err := client.GetState(ctx, "sbx"); err != nil || got != lifecycle.StateRunning {
		t.Fatalf("state = %q, want running", got)
	}
	if err := client.WriteState(ctx, "sbx", lifecycle.StatePaused, time.Minute); err != nil {
		t.Fatal(err)
	}
	updated, err = client.WriteStateCAS(
		ctx, "sbx", lifecycle.StatePaused, lifecycle.StateRunning, "9-0", time.Minute,
	)
	if err != nil || updated {
		t.Fatalf("older event WriteStateCAS() = (%v, %v), want (false, nil)", updated, err)
	}
}

func TestCursorValidDetectsTrimmedHistory(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	client := New(rdb, zap.NewNop())
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

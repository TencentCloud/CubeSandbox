// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisLeaseStoreTokenSafeRenewAndRelease(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	store := redisLeaseStore{rdb: client, key: "lease"}
	acquired, err := store.Acquire(ctx, "owner-a", 10*time.Second)
	if err != nil || !acquired {
		t.Fatalf("Acquire() = (%v, %v), want (true, nil)", acquired, err)
	}
	if acquired, err = store.Acquire(ctx, "owner-b", 10*time.Second); err != nil || acquired {
		t.Fatalf("second Acquire() = (%v, %v), want (false, nil)", acquired, err)
	}

	server.FastForward(8 * time.Second)
	renewed, err := store.Renew(ctx, "owner-a", 10*time.Second)
	if err != nil || !renewed {
		t.Fatalf("Renew(owner-a) = (%v, %v), want (true, nil)", renewed, err)
	}
	server.FastForward(5 * time.Second)
	if got, err := client.Get(ctx, "lease").Result(); err != nil || got != "owner-a" {
		t.Fatalf("renewed lease = (%q, %v), want owner-a", got, err)
	}

	if err := client.Set(ctx, "lease", "owner-b", 10*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	if renewed, err = store.Renew(ctx, "owner-a", 10*time.Second); err != nil || renewed {
		t.Fatalf("stale Renew() = (%v, %v), want (false, nil)", renewed, err)
	}
	if err := store.Release(ctx, "owner-a"); err != nil {
		t.Fatalf("stale Release() error = %v", err)
	}
	if got := client.Get(ctx, "lease").Val(); got != "owner-b" {
		t.Fatalf("stale release deleted new owner; got %q", got)
	}
	if err := store.Release(ctx, "owner-b"); err != nil {
		t.Fatalf("owner Release() error = %v", err)
	}
	if client.Exists(ctx, "lease").Val() != 0 {
		t.Fatal("owner release did not delete lease")
	}
}

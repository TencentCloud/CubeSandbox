// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redisstream

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func TestClusterStateReads(t *testing.T) {
	nodes := os.Getenv("TEST_REDIS_CLUSTER_NODES")
	if nodes == "" {
		t.Skip("TEST_REDIS_CLUSTER_NODES is not set")
	}
	rdb := redis.NewClusterClient(&redis.ClusterOptions{Addrs: strings.Split(nodes, ","), Password: os.Getenv("TEST_REDIS_CLUSTER_PASSWORD")})
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prefix := "cube-cluster-state-test:" + time.Now().Format("150405.000000000")
	a, b, missing := prefix+"{a}", prefix+"{b}", prefix+"missing"
	defer rdb.Del(context.Background(), a)
	defer rdb.Del(context.Background(), b)
	if err := rdb.Set(ctx, a, "active", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, b, "paused", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	owners := map[string]bool{}
	slots, err := rdb.ClusterSlots(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{a, b} {
		slot, err := rdb.ClusterKeySlot(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range slots {
			if slot < int64(entry.Start) || slot > int64(entry.End) {
				continue
			}
			owner := entry.Nodes[0].Addr
			owners[owner] = true
			t.Logf("lifecycle key=%s slot=%d primary=%s", key, slot, owner)
		}
	}
	if len(owners) != 2 {
		t.Fatalf("expected two primaries, got %v", owners)
	}

	values, err := New(rdb, zap.NewNop()).readStateValues(ctx, []string{b, missing, a})
	if err != nil || len(values) != 3 || values[0] != "paused" || values[1] != nil || values[2] != "active" {
		t.Fatalf("cross-slot state reads: %v %v", values, err)
	}
}

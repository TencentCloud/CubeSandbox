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
	rdb := redis.NewClusterClient(&redis.ClusterOptions{Addrs: strings.Split(nodes, ",")})
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prefix := "cube-cluster-state-test:" + time.Now().Format("150405.000000000")
	a, b, missing := prefix+"a", prefix+"b", prefix+"missing"
	defer rdb.Del(context.Background(), a)
	defer rdb.Del(context.Background(), b)
	if err := rdb.Set(ctx, a, "active", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, b, "paused", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	values, err := New(rdb, zap.NewNop()).readStateValues(ctx, []string{b, missing, a})
	if err != nil || len(values) != 3 || values[0] != "paused" || values[1] != nil || values[2] != "active" {
		t.Fatalf("cross-slot state reads: %v %v", values, err)
	}
}

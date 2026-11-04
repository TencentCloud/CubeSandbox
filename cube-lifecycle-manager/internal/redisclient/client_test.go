// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redisclient

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/config"
)

func TestClusterClientSelection(t *testing.T) {
	cfg := &config.Config{RedisClusterAddrs: []string{"127.0.0.1:17400"}}
	c := New(cfg)
	defer c.Close()
	if _, ok := c.(*redis.ClusterClient); !ok {
		t.Fatalf("client type %T", c)
	}
	if !strings.HasPrefix(DisplayAddr(cfg), "cluster:") {
		t.Fatal("missing cluster display")
	}
}

func TestClusterWatch(t *testing.T) {
	nodes := os.Getenv("TEST_REDIS_CLUSTER_NODES")
	if nodes == "" {
		t.Skip("TEST_REDIS_CLUSTER_NODES is not set")
	}
	c := New(&config.Config{RedisClusterAddrs: strings.Split(nodes, ",")})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := "cube-cluster-watch-test:" + time.Now().Format("150405.000000000")
	defer c.Del(context.Background(), key)
	if err := c.Set(ctx, key, "owner", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	err := c.Watch(ctx, func(tx *redis.Tx) error {
		owner, err := tx.Get(ctx, key).Result()
		if err != nil {
			return err
		}
		if owner != "owner" {
			t.Fatalf("unexpected owner %q", owner)
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Expire(ctx, key, 2*time.Minute); return nil })
		return err
	}, key)
	if err != nil {
		t.Fatal(err)
	}
}

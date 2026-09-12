// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

// reservationTestEnv installs a fresh node cache and scheduler config and
// restores both afterwards. Tests in this package run sequentially.
func reservationTestEnv(t *testing.T) {
	t.Helper()
	origCache := l.cache
	cfg := config.GetConfig()
	origScheduler := cfg.Scheduler
	origCubelet := cfg.CubeletConf
	origRedis := cfg.RedisConf
	t.Cleanup(func() {
		l.cache = origCache
		cfg.Scheduler = origScheduler
		cfg.CubeletConf = origCubelet
		cfg.RedisConf = origRedis
		reservationRegistry.Lock()
		reservationRegistry.nodes = make(map[string]*nodeReservationAmount)
		reservationRegistry.Unlock()
	})

	l.cache = cache.New(0, 0)
	cfg.Scheduler = &config.WrapperSchedulerConf{
		SchedulerConf: config.SchedulerConf{
			NodeMaxMvmNum:                  100,
			NodeMaxMvmNumReserveNumPercent: 1.0,
		},
	}
	cfg.CubeletConf = &config.CubeletConf{CreateTimeoutInsec: 600}
	cfg.RedisConf = nil
	reservationRegistry.Lock()
	reservationRegistry.nodes = make(map[string]*nodeReservationAmount)
	reservationRegistry.Unlock()
}

func reservationTestNode(id string) *node.Node {
	return &node.Node{
		InsID:               id,
		IP:                  "10.0.0.1",
		ReportedReady:       true,
		Healthy:             true,
		MetaDataUpdateAt:    time.Now(),
		QuotaCpu:            64000, // milli-cores
		QuotaMem:            65536, // MB
		MaxMvmLimit:         100,
		CreateConcurrentNum: 10,
	}
}

// reservationRawConn dials the test Redis directly (bypassing the wrapredis
// pool cache) so the test can inspect reservation Hashes.
func reservationRawConn(t *testing.T, addr string) redis.Conn {
	t.Helper()
	conn, err := redis.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial miniredis: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func registryAmount(t *testing.T, nodeID string) *nodeReservationAmount {
	t.Helper()
	reservationRegistry.Lock()
	defer reservationRegistry.Unlock()
	return reservationRegistry.nodes[nodeID]
}

func cachedReservedNum(t *testing.T, nodeID string) int64 {
	t.Helper()
	raw, ok := l.cache.Get(nodeID)
	if !ok {
		t.Fatalf("node %s missing from cache", nodeID)
	}
	return raw.(*node.Node).ReservedNum
}

func TestTryReserveNodeLocalOnly(t *testing.T) {
	reservationTestEnv(t)
	l.cache.SetDefault("node-r1", reservationTestNode("node-r1"))
	ctx := context.Background()

	rsv, err := TryReserveNode(ctx, "node-r1", 1000, 1024)
	if err != nil {
		t.Fatalf("TryReserveNode error: %v", err)
	}
	if rsv.redisBacked {
		t.Fatal("reservation must be local-only when Redis is not configured")
	}
	if got := cachedReservedNum(t, "node-r1"); got != 1 {
		t.Fatalf("ReservedNum=%d want 1", got)
	}
	amount := registryAmount(t, "node-r1")
	if amount == nil || amount.cpuMilli != 1000 || amount.memMB != 1024 || amount.mvm != 1 || amount.creating != 1 {
		t.Fatalf("registry amount=%+v", amount)
	}

	// CPU: raw quota is 64000, so asking for 200000 must conflict.
	if _, err := TryReserveNode(ctx, "node-r1", 200000, 1024); !errors.Is(err, ErrNodeReservationConflict) {
		t.Fatalf("cpu over-commit err=%v, want ErrNodeReservationConflict", err)
	}
	// Memory: raw quota is 65536.
	if _, err := TryReserveNode(ctx, "node-r1", 1000, 200000); !errors.Is(err, ErrNodeReservationConflict) {
		t.Fatalf("mem over-commit err=%v, want ErrNodeReservationConflict", err)
	}
	// Failed attempts must not charge the registry.
	if amount := registryAmount(t, "node-r1"); amount.cpuMilli != 1000 || amount.mvm != 1 {
		t.Fatalf("registry charged by rejected reservation: %+v", amount)
	}

	rsv.Release(ctx)
	rsv.Release(ctx) // idempotent
	if amount := registryAmount(t, "node-r1"); amount != nil {
		t.Fatalf("registry entry not cleaned: %+v", amount)
	}
	if got := cachedReservedNum(t, "node-r1"); got != 0 {
		t.Fatalf("ReservedNum=%d want 0 after release", got)
	}
	if _, err := TryReserveNode(ctx, "node-r1", 63500, 1024); err != nil {
		// 63500 only fits if the released 1000 milli-cores were actually
		// returned (free is 64000, would be 63000 on a leak).
		t.Fatalf("reservation after release should succeed: %v", err)
	}
}

func TestTryReserveNodeMvmAndCreatingLimits(t *testing.T) {
	reservationTestEnv(t)
	mvmNode := reservationTestNode("node-r2")
	mvmNode.MvmNum = 99 // limit is 100
	l.cache.SetDefault("node-r2", mvmNode)

	creatingNode := reservationTestNode("node-r3")
	creatingNode.RealTimeCreateNum = 9 // limit is 10
	l.cache.SetDefault("node-r3", creatingNode)

	ctx := context.Background()
	if _, err := TryReserveNode(ctx, "node-r2", 1000, 1024); err != nil {
		t.Fatalf("first mvm reservation should succeed: %v", err)
	}
	if _, err := TryReserveNode(ctx, "node-r2", 1000, 1024); !errors.Is(err, ErrNodeReservationConflict) {
		t.Fatalf("mvm limit err=%v, want ErrNodeReservationConflict", err)
	}
	if _, err := TryReserveNode(ctx, "node-r3", 1000, 1024); err != nil {
		t.Fatalf("first creating reservation should succeed: %v", err)
	}
	if _, err := TryReserveNode(ctx, "node-r3", 1000, 1024); !errors.Is(err, ErrNodeReservationConflict) {
		t.Fatalf("creating limit err=%v, want ErrNodeReservationConflict", err)
	}
	if _, err := TryReserveNode(ctx, "node-missing", 1000, 1024); !errors.Is(err, ErrNodeReservationConflict) {
		t.Fatalf("missing node err=%v, want ErrNodeReservationConflict", err)
	}
}

func TestTryReserveNodeRedisBacked(t *testing.T) {
	reservationTestEnv(t)
	server := miniredis.RunT(t)
	cfg := config.GetConfig()
	cfg.RedisConf = &config.RedisConf{Nodes: server.Addr(), MaxActive: 4, MaxIdle: 1, MaxRetry: 1}

	origConn := reservationRedisConn
	reservationRedisConn = func() *wrapredis.RedisWrap {
		return wrapredis.GetRedisConnPoolWrap("reservation-test", cfg.RedisConf)
	}
	t.Cleanup(func() { reservationRedisConn = origConn })

	l.cache.SetDefault("node-r4", reservationTestNode("node-r4"))
	ctx := context.Background()

	rsv, err := TryReserveNode(ctx, "node-r4", 1000, 1024)
	if err != nil {
		t.Fatalf("TryReserveNode error: %v", err)
	}
	if !rsv.redisBacked {
		t.Fatal("reservation should be redis-backed")
	}
	conn := reservationRawConn(t, server.Addr())
	fields, err := redis.StringMap(conn.Do("HGETALL", rediskey.NodeReservation("node-r4")))
	if err != nil {
		t.Fatalf("HGETALL error: %v", err)
	}
	if fields["cpu_milli"] != "1000" || fields["mem_mb"] != "1024" || fields["mvm"] != "1" || fields["creating"] != "1" {
		t.Fatalf("redis hash=%v", fields)
	}
	if ttl := server.TTL(rediskey.NodeReservation("node-r4")); ttl <= 0 {
		t.Fatalf("reservation hash has no safety TTL: %v", ttl)
	}

	// Cross-master style conflict: the Redis check rejects what would
	// overshoot effective quota (192000 milli-cores) and rolls back.
	if _, err := TryReserveNode(ctx, "node-r4", 200000, 1024); !errors.Is(err, ErrNodeReservationConflict) {
		t.Fatalf("redis conflict err=%v, want ErrNodeReservationConflict", err)
	}
	fields, _ = redis.StringMap(conn.Do("HGETALL", rediskey.NodeReservation("node-r4")))
	if fields["cpu_milli"] != "1000" {
		t.Fatalf("redis hash not rolled back: %v", fields)
	}
	if amount := registryAmount(t, "node-r4"); amount.cpuMilli != 1000 {
		t.Fatalf("local registry not rolled back: %+v", amount)
	}
	if got := cachedReservedNum(t, "node-r4"); got != 1 {
		t.Fatalf("ReservedNum=%d want 1 after rollback", got)
	}

	rsv.Release(ctx)
	fields, _ = redis.StringMap(conn.Do("HGETALL", rediskey.NodeReservation("node-r4")))
	if fields["cpu_milli"] != "0" || fields["creating"] != "0" {
		t.Fatalf("redis hash not released: %v", fields)
	}
}

func TestTryReserveNodeRedisDegraded(t *testing.T) {
	reservationTestEnv(t)
	server := miniredis.RunT(t)
	addr := server.Addr()
	server.Close() // simulate Redis going away
	cfg := config.GetConfig()
	cfg.RedisConf = &config.RedisConf{Nodes: addr, MaxActive: 1, MaxIdle: 1, MaxRetry: 1, IdleTimeout: 1}

	origConn := reservationRedisConn
	reservationRedisConn = func() *wrapredis.RedisWrap {
		return wrapredis.GetRedisConnPoolWrap("reservation-degraded-test", cfg.RedisConf)
	}
	t.Cleanup(func() { reservationRedisConn = origConn })

	l.cache.SetDefault("node-r5", reservationTestNode("node-r5"))
	ctx := context.Background()

	rsv, err := TryReserveNode(ctx, "node-r5", 1000, 1024)
	if err != nil {
		t.Fatalf("Redis outage must degrade to local-only, got: %v", err)
	}
	if rsv.redisBacked {
		t.Fatal("reservation must not be redis-backed when Redis errors")
	}
	rsv.Release(ctx)
	if amount := registryAmount(t, "node-r5"); amount != nil {
		t.Fatalf("registry entry not cleaned: %+v", amount)
	}
}

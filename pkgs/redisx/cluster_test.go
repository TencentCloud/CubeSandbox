// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redisx

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/mna/redisc"
)

func TestServerErrorsAreReturnedAsErrors(t *testing.T) {
	for _, message := range []string{"CLUSTERDOWN unavailable", "READONLY replica", "WRONGTYPE value", "OOM memory limit"} {
		t.Run(message, func(t *testing.T) {
			reply, err := normalizeReply(redis.Error(message), nil)
			if reply != nil || err == nil || err.Error() != message {
				t.Fatalf("server rejection was lost: reply=%v error=%v", reply, err)
			}
		})
	}
	want := errors.New("transport failed after write")
	if _, err := normalizeReply(nil, want); !errors.Is(err, want) {
		t.Fatalf("transport error changed: %v", err)
	}
}

func TestClusterEndpoints(t *testing.T) {
	for _, nodes := range []string{"", " , ", "redis-without-port", "redis:bad"} {
		if _, err := NewCluster(nodes, "", 1, 2); err == nil {
			t.Fatalf("accepted invalid endpoints %q", nodes)
		}
	}
}

// Promotion is opt-in because it changes the topology of the test fixture.
func TestClusterPrimaryPromotion(t *testing.T) {
	if os.Getenv("TEST_REDIS_CLUSTER_FAILOVER") != "1" {
		t.Skip("isolated failover fixture is not enabled")
	}
	nodes := os.Getenv("TEST_REDIS_CLUSTER_NODES")
	if nodes == "" {
		t.Fatal("missing Cluster seed endpoints")
	}
	seed := strings.Split(nodes, ",")[0]
	raw, err := redis.Dial("tcp", seed, redis.DialReadTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	id, err := redis.String(raw.Do("CLUSTER", "MYID"))
	if err != nil {
		t.Fatal(err)
	}
	replicas, err := redis.Strings(raw.Do("CLUSTER", "REPLICAS", id))
	if err != nil || len(replicas) == 0 {
		t.Fatalf("seed must be a primary with a replica: %v", err)
	}
	fields := strings.Fields(replicas[0])
	replica, err := redis.Dial("tcp", strings.Split(fields[1], "@")[0], redis.DialReadTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	// Choose a key in a slot owned by the seed primary.
	ranges, err := redis.Values(raw.Do("CLUSTER", "SLOTS"))
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := -1, -1
	for _, item := range ranges {
		entry, _ := redis.Values(item, nil)
		primary, _ := redis.Values(entry[2], nil)
		owner, _ := redis.String(primary[2], nil)
		if owner == id {
			lo, _ = redis.Int(entry[0], nil)
			hi, _ = redis.Int(entry[1], nil)
			break
		}
	}
	if lo < 0 {
		t.Fatal("seed slot range missing")
	}
	key := ""
	for i := 0; ; i++ {
		key = fmt.Sprintf("cube-promotion-test:%d:%d", time.Now().UnixNano(), i)
		slot := redisc.Slot(key)
		if slot >= lo && slot <= hi {
			break
		}
	}
	c, err := NewCluster(nodes, "", 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer c.Do("DEL", key)
	// Keep a second stale client whose old-primary pool can be made
	// unreachable without stopping a fixture server.
	offline, err := NewCluster(nodes, "", 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer offline.Close()
	var oldPool *redis.Pool
	createPool := offline.cluster.CreatePool
	offline.cluster.CreatePool = func(addr string, opts ...redis.DialOption) (*redis.Pool, error) {
		pool, err := createPool(addr, opts...)
		if addr == seed {
			oldPool = pool
		}
		return pool, err
	}
	// PING is a lightweight liveness probe; load the failover fixture's
	// initial topology explicitly before making its old primary unreachable.
	if err = offline.cluster.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err = offline.Do("PING"); err != nil {
		t.Fatal(err)
	}
	if oldPool == nil {
		t.Fatal("old primary pool missing")
	}
	if _, err = c.Do("PING"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Do("SET", key, "0"); err != nil {
		t.Fatal(err)
	}
	if _, err = replica.Do("CLUSTER", "FAILOVER"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		role, e := redis.Values(raw.Do("ROLE"))
		if e != nil {
			t.Fatal(e)
		}
		name, _ := redis.String(role[0], nil)
		if name != "master" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica promotion timed out")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The client retains the old slot map and must follow MOVED without
	// applying the increment more than once.
	n, err := redis.Int(c.Do("INCR", key))
	if err != nil || n != 1 {
		t.Fatalf("write after promotion: %d %v", n, err)
	}
	oldPool.Close()
	// When establishing a connection to the old primary fails, redisc falls
	// back to another known primary, which redirects to the new slot owner.
	value, err := redis.String(offline.Do("GET", key))
	if err != nil || value != "1" {
		t.Fatalf("unreachable primary topology recovery: %q %v", value, err)
	}
}

// Run against an isolated local Cluster: TEST_REDIS_CLUSTER_NODES lists seeds.
func TestClusterCommands(t *testing.T) {
	nodes := os.Getenv("TEST_REDIS_CLUSTER_NODES")
	if nodes == "" {
		t.Skip("TEST_REDIS_CLUSTER_NODES is not set")
	}
	c, err := NewCluster(nodes, "", 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	prefix := fmt.Sprintf("cube-cluster-test:%d:", time.Now().UnixNano())
	a, b, missing := prefix+"a", prefix+"b", prefix+"missing"
	defer c.Do("DEL", a, b)
	for key, value := range map[string]string{a: "one", b: "two"} {
		if _, err := c.Do("SET", key, value); err != nil {
			t.Fatal(err)
		}
	}
	values, err := redis.Values(c.Do("MGET", b, missing, a))
	if err != nil || len(values) != 3 {
		t.Fatalf("MGET: %v %v", values, err)
	}
	first, _ := redis.String(values[0], nil)
	last, _ := redis.String(values[2], nil)
	if first != "two" || values[1] != nil || last != "one" {
		t.Fatalf("MGET order/misses: %v", values)
	}
	keys, err := c.ScanKeys(prefix + "*")
	if err != nil || len(keys) != 2 {
		t.Fatalf("scan primaries: %v %v", keys, err)
	}
	// The script's first argument is not its key. Incorrect generic routing
	// would fail here instead of selecting the sandbox key's slot.
	result, err := redis.String(c.Do("EVAL", "return redis.call('GET',KEYS[1])", 1, b))
	if err != nil || result != "two" {
		t.Fatalf("script route: %q %v", result, err)
	}
	if _, err = c.Do("EVAL", "return 1", 2, a, b); err == nil {
		t.Fatal("accepted cross-slot script")
	}
	lock := prefix + "lock"
	if _, err = c.Do("SET", lock, "owner", "NX", "PX", 30000); err != nil {
		t.Fatal(err)
	}
	unlocked, err := redis.Int(c.Do("EVAL", "if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) else return 0 end", 1, lock, "owner"))
	if err != nil || unlocked != 1 {
		t.Fatalf("unlock: %d %v", unlocked, err)
	}
	deleted, err := redis.Int(c.Do("DEL", a, b))
	if err != nil || deleted != 2 {
		t.Fatalf("cross-slot cleanup: %d %v", deleted, err)
	}
	if _, err = c.Do("SELECT", 1); err == nil {
		t.Fatal("accepted DB 1")
	}
}

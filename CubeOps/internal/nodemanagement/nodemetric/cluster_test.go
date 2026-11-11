// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemetric

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

func TestClusterSnapshots(t *testing.T) {
	nodes := os.Getenv("TEST_REDIS_CLUSTER_NODES")
	if nodes == "" {
		t.Skip("TEST_REDIS_CLUSTER_NODES is not set")
	}
	if pool != nil {
		t.Fatal("expected isolated pool")
	}
	poolOnce = sync.Once{}
	if err := Init(&config.Config{RedisClusterNodes: nodes, RedisPassword: os.Getenv("TEST_REDIS_CLUSTER_PASSWORD")}); err != nil {
		t.Fatal(err)
	}
	defer func() { pool.Close(); clusterClient.Close(); pool = nil; clusterClient = nil; poolOnce = sync.Once{} }()
	prefix := "cluster-node-test-" + time.Now().Format("150405.000000000")
	a, b := prefix+"{a}", prefix+"{b}"
	WriteNodeSnapshot(&model.NodeSnapshot{NodeID: a, HostIP: "127.0.0.1"})
	WriteNodeSnapshot(&model.NodeSnapshot{NodeID: b, HostIP: "127.0.0.2"})
	defer DeleteNodeSnapshot(a)
	defer DeleteNodeSnapshot(b)
	all, err := ScanNodeSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, snap := range all {
		found[snap.NodeID] = true
	}

	owners := map[string]bool{}
	slots, err := redis.Values(clusterClient.Do("CLUSTER", "SLOTS"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{SnapshotKeyForTest(a), SnapshotKeyForTest(b)} {
		slot, err := redis.Int(clusterClient.Do("CLUSTER", "KEYSLOT", key))
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range slots {
			entry, _ := redis.Values(raw, nil)
			lo, _ := redis.Int(entry[0], nil)
			hi, _ := redis.Int(entry[1], nil)
			if slot < lo || slot > hi {
				continue
			}
			node, _ := redis.Values(entry[2], nil)
			host, _ := redis.String(node[0], nil)
			port, _ := redis.Int(node[1], nil)
			owner := fmt.Sprintf("%s:%d", host, port)
			owners[owner] = true
			t.Logf("snapshot key=%s slot=%d primary=%s found_by_scan=true", key, slot, owner)
		}
	}
	if len(owners) != 2 {
		t.Fatalf("expected two primaries, got %v", owners)
	}

	if !found[a] || !found[b] {
		t.Fatalf("scan missed nodes: %v", found)
	}
}

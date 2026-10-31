// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemetric

import (
	"os"
	"sync"
	"testing"
	"time"

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
	if err := Init(&config.Config{RedisClusterNodes: nodes}); err != nil {
		t.Fatal(err)
	}
	defer func() { pool.Close(); clusterClient.Close(); pool = nil; clusterClient = nil; poolOnce = sync.Once{} }()
	prefix := "cluster-node-test-" + time.Now().Format("150405.000000000")
	a, b := prefix+"a", prefix+"b"
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
	if !found[a] || !found[b] {
		t.Fatalf("scan missed nodes: %v", found)
	}
}

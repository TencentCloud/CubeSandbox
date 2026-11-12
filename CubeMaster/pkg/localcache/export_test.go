// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/nodehealth"
)

func TestGetHealthyNodesByInstanceType(t *testing.T) {

	origNodesByClusters := l.sortedNodesByClusters
	origCache := l.cache
	defer func() {
		l.sortedNodesByClusters = origNodesByClusters
		l.cache = origCache
	}()

	createNodes := func(count int, healthy bool) node.NodeList {
		nodes := make(node.NodeList, count)
		for i := 0; i < count; i++ {
			nodes[i] = &node.Node{
				ReportedReady:    healthy,
				Healthy:          healthy,
				MetaDataUpdateAt: time.Now(),
			}
		}
		return nodes
	}

	tests := []struct {
		name    string
		prepare func()
		args    struct {
			n       int
			product string
		}
		want node.NodeList
	}{
		{
			name: "产品类型不存在",
			prepare: func() {
				l.sortedNodesByClusters = map[string]node.NodeList{
					"other": createNodes(3, true),
				}
				l.cache = cache.New(0, 0)
			},
			args: struct {
				n       int
				product string
			}{n: 2, product: "invalid"},
			want: node.NodeList{},
		},
		{
			name: "n=-1 返回全部节点",
			prepare: func() {
				l.sortedNodesByClusters = map[string]node.NodeList{
					"valid": createNodes(5, true),
				}
			},
			args: struct {
				n       int
				product string
			}{n: -1, product: "valid"},
			want: createNodes(5, true),
		},
		{
			name: "n=0 返回空列表",
			prepare: func() {
				l.sortedNodesByClusters = map[string]node.NodeList{
					"valid": createNodes(5, true),
				}
			},
			args: struct {
				n       int
				product string
			}{n: 0, product: "valid"},
			want: node.NodeList{},
		},
		{
			name: "健康节点不足",
			prepare: func() {
				l.sortedNodesByClusters = map[string]node.NodeList{
					"valid": append(createNodes(2, true), createNodes(3, false)...),
				}
			},
			args: struct {
				n       int
				product string
			}{n: 5, product: "valid"},
			want: createNodes(2, true),
		},
		{
			name: "健康节点足够",
			prepare: func() {
				l.sortedNodesByClusters = map[string]node.NodeList{
					"valid": append(createNodes(5, true), createNodes(2, false)...),
				}
			},
			args: struct {
				n       int
				product string
			}{n: 3, product: "valid"},
			want: createNodes(3, true),
		},
		{
			name: "节点列表为空",
			prepare: func() {
				l.sortedNodesByClusters = map[string]node.NodeList{
					"empty": {},
				}
			},
			args: struct {
				n       int
				product string
			}{n: 3, product: "empty"},
			want: node.NodeList{},
		},
		{
			name: "n为负数(非-1)",
			prepare: func() {
				l.sortedNodesByClusters = map[string]node.NodeList{
					"valid": append(createNodes(3, true), createNodes(2, false)...),
				}
			},
			args: struct {
				n       int
				product string
			}{n: -2, product: "valid"},
			want: createNodes(3, true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.prepare()

			got := GetHealthyNodesByInstanceType(tt.args.n, tt.args.product)

			if len(got) != len(tt.want) {
				t.Fatalf("长度不符: got %d, want %d", len(got), len(tt.want))
			}

			for i := 0; i < len(got); i++ {
				if got[i].Healthy != tt.want[i].Healthy {
					t.Errorf("节点健康状态错误: got %v, want %v", got[i].Healthy, tt.want[i].Healthy)
				}
			}
		})
	}
}

func isolatePackageLocality(t *testing.T) {
	t.Helper()
	origCache := l.cache
	origLocality := l.locality
	l.cache = cache.New(0, 0)
	l.locality = newTemplateLocality()
	t.Cleanup(func() {
		l.cache = origCache
		l.locality = origLocality
	})
}

func TestSyncNodeTemplatesReconcilesHeartbeatState(t *testing.T) {
	isolatePackageLocality(t)
	l.cache.SetDefault("node-a", &node.Node{InsID: "node-a", IP: "127.0.0.1", Healthy: true})

	ctx := context.Background()
	SyncNodeTemplates(ctx, "node-a", []string{"tpl-old", "tpl-keep"})
	SyncNodeTemplates(ctx, "node-a", []string{"tpl-keep", "tpl-new"})

	if state := GetImageStateByNode("tpl-old", "node-a"); state != nil {
		t.Fatal("tpl-old should be removed from node locality after heartbeat sync")
	}
	if state := GetImageStateByNode("tpl-keep", "node-a"); state == nil {
		t.Fatal("tpl-keep should remain in node locality after heartbeat sync")
	}
	if state := GetImageStateByNode("tpl-new", "node-a"); state == nil {
		t.Fatal("tpl-new should be added to node locality after heartbeat sync")
	}

	RegisterTemplateReplica("tpl-reg", "node-a", 1)
	SyncNodeTemplates(ctx, "node-a", []string{"tpl-new"})
	if state := GetImageStateByNode("tpl-reg", "node-a"); state == nil {
		t.Fatal("template-center registration must survive a heartbeat that omits it")
	}
	if state := GetImageStateByNode("tpl-keep", "node-a"); state != nil {
		t.Fatal("heartbeat-only tpl-keep should be removed when the next heartbeat omits it")
	}
}

func TestInvalidateImageStateKeepsReportedLocality(t *testing.T) {
	isolatePackageLocality(t)
	l.cache.SetDefault("node-a", &node.Node{InsID: "node-a", IP: "127.0.0.1", Healthy: true})
	l.cache.SetDefault("node-b", &node.Node{InsID: "node-b", IP: "127.0.0.2", Healthy: true})

	ctx := context.Background()
	SyncNodeTemplates(ctx, "node-b", []string{"tpl"})
	RegisterTemplateReplica("tpl", "node-b", 1)
	RegisterTemplateReplica("tpl", "node-a", 1)

	InvalidateImageState("tpl")

	if state := GetImageStateByNode("tpl", "node-a"); state != nil {
		t.Fatal("invalidation must drop a node that was only registered")
	}
	if state := GetImageStateByNode("tpl", "node-b"); state == nil {
		t.Fatal("invalidation must keep a node the heartbeat still reports")
	}
}

func TestGetNodeTrustsCubeOpsHealthVerdict(t *testing.T) {
	origCache := l.cache
	defer func() {
		l.cache = origCache
	}()

	l.cache = cache.New(0, 0)
	// CubeOps marked this node unhealthy; sync is fresh so verdict is trusted.
	l.cache.SetDefault("node-unhealthy", &node.Node{
		InsID:            "node-unhealthy",
		IP:               "10.0.0.1",
		ReportedReady:    true,
		Healthy:          false,
		UnhealthyReason:  nodehealth.ReasonHeartbeatExpired,
		MetaDataUpdateAt: time.Now(),
	})

	got, ok := GetNode("node-unhealthy")
	if !ok || got == nil {
		t.Fatal("expected node to exist")
	}
	if got.Healthy {
		t.Fatal("should trust CubeOps unhealthy verdict")
	}
	if got.UnhealthyReason != nodehealth.ReasonHeartbeatExpired {
		t.Fatalf("UnhealthyReason=%s want %s", got.UnhealthyReason, nodehealth.ReasonHeartbeatExpired)
	}
}

func TestGetNodeTrustsCubeOpsHealthOnStaleSync(t *testing.T) {
	origCache := l.cache
	defer func() {
		l.cache = origCache
	}()

	l.cache = cache.New(0, 0)
	// Trust CubeOps Healthy verdict even when CubeOps sync is stale.
	l.cache.SetDefault("node-stale-sync", &node.Node{
		InsID:            "node-stale-sync",
		IP:               "10.0.0.1",
		ReportedReady:    true,
		Healthy:          true,
		MetaDataUpdateAt: time.Now().Add(-time.Hour), // long-stale sync
	})

	got, ok := GetNode("node-stale-sync")
	if !ok || got == nil {
		t.Fatal("expected node to exist")
	}
	if !got.Healthy {
		t.Fatal("should keep CubeOps Healthy verdict even on stale sync")
	}
}

func TestGetHealthyNodesByInstanceTypeTrustsCubeOpsHealth(t *testing.T) {
	origNodesByClusters := l.sortedNodesByClusters
	defer func() {
		l.sortedNodesByClusters = origNodesByClusters
	}()

	now := time.Now()
	fresh := &node.Node{
		InsID:            "node-fresh",
		ReportedReady:    true,
		Healthy:          true,
		MetaDataUpdateAt: now,
	}
	stale := &node.Node{
		InsID:            "node-stale",
		ReportedReady:    true,
		Healthy:          false, // CubeOps already marked this unhealthy
		MetaDataUpdateAt: now,
	}
	l.sortedNodesByClusters = map[string]node.NodeList{
		"valid": {fresh, stale},
	}

	got := GetHealthyNodesByInstanceType(-1, "valid")
	if got.Len() != 1 {
		t.Fatalf("healthy node count=%d want 1", got.Len())
	}
	if got[0].ID() != fresh.ID() {
		t.Fatalf("healthy node=%s want %s", got[0].ID(), fresh.ID())
	}
}

func TestGetNodesByIpTrustsCubeOpsHealthVerdict(t *testing.T) {
	origCache := l.cache
	defer func() {
		l.cache = origCache
	}()

	l.cache = cache.New(0, 0)
	l.cache.SetDefault("node-unhealthy", &node.Node{
		InsID:            "node-unhealthy",
		IP:               "10.0.0.9",
		ReportedReady:    true,
		Healthy:          false,
		UnhealthyReason:  nodehealth.ReasonHeartbeatExpired,
		MetaDataUpdateAt: time.Now(),
	})

	got, ok := GetNodesByIp("10.0.0.9")
	if !ok || got == nil {
		t.Fatal("expected node to exist")
	}
	if got.Healthy {
		t.Fatal("should trust CubeOps unhealthy verdict")
	}
	if got.UnhealthyReason != nodehealth.ReasonHeartbeatExpired {
		t.Fatalf("UnhealthyReason=%s want %s", got.UnhealthyReason, nodehealth.ReasonHeartbeatExpired)
	}
}

func TestNodeConcurrentCountersUpdateCachedNodeFromReadClone(t *testing.T) {
	origCache := l.cache
	defer func() {
		l.cache = origCache
	}()

	l.cache = cache.New(0, 0)
	l.cache.SetDefault("node-a", &node.Node{
		InsID:            "node-a",
		IP:               "10.0.0.10",
		ReportedReady:    true,
		Healthy:          true,
		MetaDataUpdateAt: time.Now(),
	})

	got, ok := GetNode("node-a")
	if !ok || got == nil {
		t.Fatal("expected node to exist")
	}
	if err := IncrNodeConcurrent(got); err != nil {
		t.Fatalf("IncrNodeConcurrent error: %v", err)
	}
	if err := DecrNodeConcurrent(got); err != nil {
		t.Fatalf("DecrNodeConcurrent error: %v", err)
	}

	raw, ok := l.cache.Get("node-a")
	if !ok {
		t.Fatal("expected cached node to remain in cache")
	}
	cached, ok := raw.(*node.Node)
	if !ok {
		t.Fatal("expected cached node type")
	}
	if cached.LocalCreateNum != 0 {
		t.Fatalf("LocalCreateNum=%d want 0", cached.LocalCreateNum)
	}
}

func TestSyncNodeTemplates_EmptyListCleansUp(t *testing.T) {
	isolatePackageLocality(t)
	l.cache.SetDefault("node-a", &node.Node{InsID: "node-a", IP: "127.0.0.1", Healthy: true})

	ctx := context.Background()
	SyncNodeTemplates(ctx, "node-a", []string{"tpl-old", "tpl-stale"})
	RegisterTemplateReplica("tpl-reg", "node-a", 1)

	SyncNodeTemplates(ctx, "node-a", []string{})

	if state := GetImageStateByNode("tpl-old", "node-a"); state != nil {
		t.Fatal("tpl-old should be removed after empty heartbeat")
	}
	if state := GetImageStateByNode("tpl-stale", "node-a"); state != nil {
		t.Fatal("tpl-stale should be removed after empty heartbeat")
	}
	if state := GetImageStateByNode("tpl-reg", "node-a"); state == nil {
		t.Fatal("registered replica must survive an empty heartbeat")
	}
	if reported := l.locality.reported["node-a"]; reported.Len() != 0 {
		t.Fatalf("expected empty heartbeat inventory, got %v", reported)
	}
}

func TestSyncAllFromDB_ExternalLoaderSyncsTemplates(t *testing.T) {
	origLoader := externalNodeLoader
	origSorted := l.sortedNodesByClusters
	isolatePackageLocality(t)
	t.Cleanup(func() {
		externalNodeLoader = origLoader
		l.sortedNodesByClusters = origSorted
	})

	l.sortedNodesByClusters = map[string]node.NodeList{
		constants.DefaultInstanceTypeName: {},
	}

	externalNodeLoader = func(_ context.Context) ([]*node.Node, error) {
		return []*node.Node{
			{
				InsID:            "node-1",
				IP:               "10.0.0.1",
				Healthy:          true,
				MetaDataUpdateAt: time.Now(),
				LocalTemplates:   []string{"tpl-1", "tpl-2"},
			},
		}, nil
	}

	if err := l.syncAllFromDB(context.Background(), false); err != nil {
		t.Fatalf("syncAllFromDB: %v", err)
	}

	n, ok := GetNode("node-1")
	if !ok || n == nil {
		t.Fatal("expected node-1 in cache")
	}
	if n.IP != "10.0.0.1" {
		t.Errorf("IP = %s, want 10.0.0.1", n.IP)
	}
	if state := GetImageStateByNode("tpl-1", "node-1"); state == nil {
		t.Fatal("expected tpl-1 locality")
	}
	if state := GetImageStateByNode("tpl-2", "node-1"); state == nil {
		t.Fatal("expected tpl-2 locality")
	}
}

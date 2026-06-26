// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
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

func TestSyncNodeTemplatesReconcilesHeartbeatState(t *testing.T) {
	origCache := l.cache
	origImageCache := l.imageCache
	origTemplateNodeCache := l.templateNodeCache
	defer func() {
		l.cache = origCache
		l.imageCache = origImageCache
		l.templateNodeCache = origTemplateNodeCache
	}()

	l.cache = cache.New(0, 0)
	l.imageCache = cache.New(0, 0)
	l.templateNodeCache = cache.New(0, 0)
	l.cache.SetDefault("node-a", &node.Node{InsID: "node-a", IP: "127.0.0.1", Healthy: true})

	RegisterTemplateReplica("tpl-old", "node-a", 1)
	RegisterTemplateReplica("tpl-keep", "node-a", 1)

	SyncNodeTemplates("node-a", []string{"tpl-keep", "tpl-new"})

	if state := GetImageStateByNode("tpl-old", "node-a"); state != nil {
		t.Fatal("tpl-old should be removed from node locality after heartbeat sync")
	}
	if state := GetImageStateByNode("tpl-keep", "node-a"); state == nil {
		t.Fatal("tpl-keep should remain in node locality after heartbeat sync")
	}
	if state := GetImageStateByNode("tpl-new", "node-a"); state == nil {
		t.Fatal("tpl-new should be added to node locality after heartbeat sync")
	}
	if templates, ok := getCachedNodeTemplateSet("node-a"); !ok {
		t.Fatal("expected node template membership cache to be populated")
	} else {
		if _, ok := templates["tpl-old"]; ok {
			t.Fatal("stale template membership should be removed from reverse index")
		}
		if _, ok := templates["tpl-keep"]; !ok {
			t.Fatal("tpl-keep should be present in reverse index")
		}
		if _, ok := templates["tpl-new"]; !ok {
			t.Fatal("tpl-new should be present in reverse index")
		}
	}
}

func TestSyncNodeTemplatesDiscoversWarmStateWithoutReverseIndex(t *testing.T) {
	origImageCache := l.imageCache
	origTemplateNodeCache := l.templateNodeCache
	defer func() {
		l.imageCache = origImageCache
		l.templateNodeCache = origTemplateNodeCache
	}()

	l.imageCache = cache.New(0, 0)
	l.templateNodeCache = cache.New(0, 0)
	l.addImageCache("tpl-stale", fwk.NewImageStateSummary(1, "", "node-a"))
	l.addImageCache("tpl-keep", fwk.NewImageStateSummary(1, "", "node-a"))

	SyncNodeTemplates("node-a", []string{"tpl-keep"})

	if state := GetImageStateByNode("tpl-stale", "node-a"); state != nil {
		t.Fatal("tpl-stale should be removed when syncing from discovered warm cache state")
	}
	if state := GetImageStateByNode("tpl-keep", "node-a"); state == nil {
		t.Fatal("tpl-keep should remain after syncing from discovered warm cache state")
	}
}

func TestInvalidateImageStateAllowsHeartbeatToRebuildLocality(t *testing.T) {
	origCache := l.cache
	origImageCache := l.imageCache
	origTemplateNodeCache := l.templateNodeCache
	defer func() {
		l.cache = origCache
		l.imageCache = origImageCache
		l.templateNodeCache = origTemplateNodeCache
	}()

	l.cache = cache.New(0, 0)
	l.imageCache = cache.New(0, 0)
	l.templateNodeCache = cache.New(0, 0)
	l.cache.SetDefault("node-a", &node.Node{InsID: "node-a", IP: "127.0.0.1", Healthy: true})

	RegisterTemplateReplica("tpl-replay", "node-a", 1)
	if _, ok := getCachedNodeTemplateSet("node-a"); !ok {
		t.Fatal("expected reverse index before invalidation")
	}

	InvalidateImageState("tpl-replay")

	if state := GetImageStateByNode("tpl-replay", "node-a"); state != nil {
		t.Fatal("image cache should be empty immediately after invalidation")
	}
	if templates, ok := getCachedNodeTemplateSet("node-a"); !ok {
		t.Fatal("expected reverse index entry to remain addressable after invalidation cleanup")
	} else if _, exists := templates["tpl-replay"]; exists {
		t.Fatal("reverse index should drop invalidated template membership")
	}

	SyncNodeTemplates("node-a", []string{"tpl-replay"})

	if state := GetImageStateByNode("tpl-replay", "node-a"); state == nil {
		t.Fatal("heartbeat replay should rebuild template locality after invalidation")
	}
	if templates, ok := getCachedNodeTemplateSet("node-a"); !ok {
		t.Fatal("expected reverse index after heartbeat replay")
	} else if _, exists := templates["tpl-replay"]; !exists {
		t.Fatal("reverse index should be rebuilt after heartbeat replay")
	}
}

func TestGetNodeRefreshesCurrentHealthFromCachedFacts(t *testing.T) {
	origCache := l.cache
	defer func() {
		l.cache = origCache
	}()

	l.cache = cache.New(0, 0)
	staleHeartbeat := time.Now().Add(-(metadataHealthTimeout() + time.Second))
	l.cache.SetDefault("node-stale", &node.Node{
		InsID:            "node-stale",
		IP:               "10.0.0.1",
		ReportedReady:    true,
		Healthy:          true,
		MetaDataUpdateAt: staleHeartbeat,
	})

	got, ok := GetNode("node-stale")
	if !ok || got == nil {
		t.Fatal("expected node to exist")
	}
	if got.Healthy {
		t.Fatal("stale heartbeat should be reflected as unhealthy")
	}
	if got.UnhealthyReason != nodehealth.ReasonHeartbeatExpired {
		t.Fatalf("UnhealthyReason=%s want %s", got.UnhealthyReason, nodehealth.ReasonHeartbeatExpired)
	}
	raw, ok := l.cache.Get("node-stale")
	if !ok {
		t.Fatal("expected cached node to remain in cache")
	}
	cached, ok := raw.(*node.Node)
	if !ok {
		t.Fatal("expected cached node type")
	}
	if !cached.Healthy {
		t.Fatal("read path should not mutate cached health state")
	}
}

func TestGetHealthyNodesByInstanceTypeFiltersExpiredHeartbeat(t *testing.T) {
	origNodesByClusters := l.sortedNodesByClusters
	defer func() {
		l.sortedNodesByClusters = origNodesByClusters
	}()

	fresh := &node.Node{
		InsID:            "node-fresh",
		ReportedReady:    true,
		Healthy:          true,
		MetaDataUpdateAt: time.Now(),
	}
	stale := &node.Node{
		InsID:            "node-stale",
		ReportedReady:    true,
		Healthy:          true,
		MetaDataUpdateAt: time.Now().Add(-(metadataHealthTimeout() + time.Second)),
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
	if !stale.Healthy {
		t.Fatal("read path should not mutate source node health state")
	}
}

func TestGetNodesByIpRefreshesCurrentHealthFromCachedFacts(t *testing.T) {
	origCache := l.cache
	defer func() {
		l.cache = origCache
	}()

	l.cache = cache.New(0, 0)
	staleHeartbeat := time.Now().Add(-(metadataHealthTimeout() + time.Second))
	l.cache.SetDefault("node-stale", &node.Node{
		InsID:            "node-stale",
		IP:               "10.0.0.9",
		ReportedReady:    true,
		Healthy:          true,
		MetaDataUpdateAt: staleHeartbeat,
	})

	got, ok := GetNodesByIp("10.0.0.9")
	if !ok || got == nil {
		t.Fatal("expected node to exist")
	}
	if got.Healthy {
		t.Fatal("stale heartbeat should be reflected as unhealthy")
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

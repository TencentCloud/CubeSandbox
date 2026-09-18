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

	heartbeat := time.Now()
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep", "tpl-new"}, heartbeat)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep", "tpl-new"}, heartbeat.Add(time.Second))
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep", "tpl-new"}, heartbeat.Add(2*time.Second))

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

func TestSyncNodeTemplatesRetainsDirectRegistrationAcrossStaleOmission(t *testing.T) {
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

	h1 := time.Unix(100, 0)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-base"}, h1)
	RegisterTemplateReplica("snap-new", "node-a", 1)

	h2 := h1.Add(time.Second)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-base"}, h2)
	if state := GetImageStateByNode("snap-new", "node-a"); state == nil {
		t.Fatal("one heartbeat omission erased a directly registered snapshot")
	}

	// CubeMaster can poll the same CubeOps heartbeat multiple times. Replays must
	// not count as additional omission confirmations.
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-base"}, h2)
	if state := GetImageStateByNode("snap-new", "node-a"); state == nil {
		t.Fatal("duplicate heartbeat erased a directly registered snapshot")
	}

	// Once Cubelet's next heartbeat includes the snapshot, the pending omission
	// is cleared and an older response cannot undo that observation.
	h3 := h2.Add(time.Second)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-base", "snap-new"}, h3)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-base"}, h2)
	if state := GetImageStateByNode("snap-new", "node-a"); state == nil {
		t.Fatal("out-of-order heartbeat erased confirmed snapshot locality")
	}
}

func TestPreRegistrationHeartbeatsCannotEraseDirectRegistration(t *testing.T) {
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

	preRegistration := time.Now().Add(-time.Second)
	SyncNodeTemplates(context.Background(), "node-a", nil, preRegistration)
	RegisterTemplateReplica("snap-new", "node-a", 1)
	SyncNodeTemplates(context.Background(), "node-a", nil, preRegistration)
	if state := GetImageStateByNode("snap-new", "node-a"); state == nil {
		t.Fatal("replayed heartbeat captured before direct registration erased snapshot locality")
	}

	postRegistration := time.Now().Add(time.Second)
	SyncNodeTemplates(context.Background(), "node-a", nil, postRegistration)
	if state := GetImageStateByNode("snap-new", "node-a"); state == nil {
		t.Fatal("first post-registration omission should be pending")
	}
	SyncNodeTemplates(context.Background(), "node-a", nil, postRegistration.Add(time.Second))
	if state := GetImageStateByNode("snap-new", "node-a"); state == nil {
		t.Fatal("second post-registration omission should be pending after barrier pass")
	}
	SyncNodeTemplates(context.Background(), "node-a", nil, postRegistration.Add(2*time.Second))
	if state := GetImageStateByNode("snap-new", "node-a"); state != nil {
		t.Fatal("third post-registration omission should remove stale locality")
	}
}

func TestRepeatedRegistrationDoesNotRearmRemovalBarrier(t *testing.T) {
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

	h1 := time.Unix(140, 0)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-old"}, h1)
	SyncNodeTemplates(context.Background(), "node-a", nil, h1.Add(time.Second))
	RegisterTemplateReplica("tpl-old", "node-a", 1)
	SyncNodeTemplates(context.Background(), "node-a", nil, h1.Add(2*time.Second))
	if state := GetImageStateByNode("tpl-old", "node-a"); state != nil {
		t.Fatal("re-registering known locality should not reset pending removal")
	}
}

func TestRepeatedRegistrationRefreshesImageStateWithoutClobberingSize(t *testing.T) {
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
	l.cache.SetDefault("node-a", &node.Node{
		InsID:           "node-a",
		IP:              "127.0.0.1",
		Healthy:         true,
		OssClusterLabel: "cluster-a",
	})
	RegisterTemplateReplica("tpl-old", "node-a", 1024)
	state := GetImageStateByNode("tpl-old", "node-a")
	if state == nil {
		t.Fatal("expected registered image state")
	}
	oldUpdate := state.UpdateAt
	state.ScaledImageScore = 0
	time.Sleep(time.Millisecond)

	RegisterTemplateReplica("tpl-old", "node-a", 1)
	if state.Size != 1024 {
		t.Fatalf("repeated metadata registration clobbered size: %d", state.Size)
	}
	if !state.UpdateAt.After(oldUpdate) {
		t.Fatalf("repeated registration did not refresh UpdateAt: old=%v new=%v", oldUpdate, state.UpdateAt)
	}
	if state.ScaledImageScore == 0 {
		t.Fatal("repeated registration did not refresh ScaledImageScore")
	}
}

func TestDirectRegistrationPreservesDiscoveredWarmState(t *testing.T) {
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
	l.addImageCache("tpl-stale", fwk.NewImageStateSummary(1, "", "node-a"))

	RegisterTemplateReplica("tpl-new", "node-a", 1)
	h1 := time.Unix(175, 0)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-new"}, h1)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-new"}, h1.Add(time.Second))
	if state := GetImageStateByNode("tpl-stale", "node-a"); state != nil {
		t.Fatal("direct registration lost warm reverse state needed for reconciliation")
	}
}

func TestSyncNodeTemplatesStaleObservationIsAdditiveOnly(t *testing.T) {
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

	high := time.Unix(300, 0)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep", "tpl-missing"}, high)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep"}, high.Add(time.Second))
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep", "tpl-added"}, high.Add(-time.Second))

	if state := GetImageStateByNode("tpl-added", "node-a"); state == nil {
		t.Fatal("stale observation should still add reported locality")
	}
	if state := GetImageStateByNode("tpl-missing", "node-a"); state == nil {
		t.Fatal("stale observation should not confirm a pending omission")
	}

	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep", "tpl-added"}, high.Add(2*time.Second))
	if state := GetImageStateByNode("tpl-missing", "node-a"); state == nil {
		t.Fatal("first recovered observation should restart omission confirmation")
	}
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep", "tpl-added"}, high.Add(3*time.Second))
	if state := GetImageStateByNode("tpl-missing", "node-a"); state != nil {
		t.Fatal("second recovered observation should confirm the omission")
	}
}

func TestSyncNodeTemplatesRequiresTwoDistinctOmissions(t *testing.T) {
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

	h1 := time.Unix(200, 0)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-delete"}, h1)
	SyncNodeTemplates(context.Background(), "node-a", nil, h1.Add(time.Second))
	if state := GetImageStateByNode("tpl-delete", "node-a"); state == nil {
		t.Fatal("first heartbeat omission should retain locality pending confirmation")
	}
	SyncNodeTemplates(context.Background(), "node-a", nil, h1.Add(2*time.Second))
	if state := GetImageStateByNode("tpl-delete", "node-a"); state != nil {
		t.Fatal("second distinct heartbeat omission should remove locality")
	}
}

func TestSyncNodeTemplatesZeroTimestampIsAdditiveOnly(t *testing.T) {
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
	RegisterTemplateReplica("tpl-existing", "node-a", 1)

	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-added"}, time.Time{})
	if state := GetImageStateByNode("tpl-existing", "node-a"); state == nil {
		t.Fatal("timestamp-less heartbeat should not remove omitted locality")
	}
	if state := GetImageStateByNode("tpl-added", "node-a"); state == nil {
		t.Fatal("timestamp-less heartbeat should still add reported locality")
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

	heartbeat := time.Now()
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep"}, heartbeat)
	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-keep"}, heartbeat.Add(time.Second))

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

	SyncNodeTemplates(context.Background(), "node-a", []string{"tpl-replay"}, time.Now())

	if state := GetImageStateByNode("tpl-replay", "node-a"); state == nil {
		t.Fatal("heartbeat replay should rebuild template locality after invalidation")
	}
	if templates, ok := getCachedNodeTemplateSet("node-a"); !ok {
		t.Fatal("expected reverse index after heartbeat replay")
	} else if _, exists := templates["tpl-replay"]; !exists {
		t.Fatal("reverse index should be rebuilt after heartbeat replay")
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
	RegisterTemplateReplica("tpl-stale", "node-a", 1)

	heartbeat := time.Now()
	SyncNodeTemplates(context.Background(), "node-a", []string{}, heartbeat)
	SyncNodeTemplates(context.Background(), "node-a", []string{}, heartbeat.Add(time.Second))
	SyncNodeTemplates(context.Background(), "node-a", []string{}, heartbeat.Add(2*time.Second))

	if state := GetImageStateByNode("tpl-old", "node-a"); state != nil {
		t.Fatal("tpl-old should be removed after empty heartbeat")
	}
	if state := GetImageStateByNode("tpl-stale", "node-a"); state != nil {
		t.Fatal("tpl-stale should be removed after empty heartbeat")
	}
	if templates, ok := getCachedNodeTemplateSet("node-a"); !ok || len(templates) != 0 {
		t.Fatalf("expected empty template set, got %v", templates)
	}
}

func TestDeregisterTemplateReplicaIsImmediate(t *testing.T) {
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
	RegisterTemplateReplica("tpl-delete", "node-a", 1)

	DeregisterTemplateReplica("tpl-delete", "node-a")
	if state := GetImageStateByNode("tpl-delete", "node-a"); state != nil {
		t.Fatal("explicit deregistration should not wait for heartbeat confirmation")
	}
}

func TestForceRemoveNodeTemplatesIsImmediate(t *testing.T) {
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
	RegisterTemplateReplica("tpl-delete", "node-a", 1)

	forceRemoveNodeTemplates(context.Background(), "node-a")
	if state := GetImageStateByNode("tpl-delete", "node-a"); state != nil {
		t.Fatal("node removal should immediately clear template locality")
	}
	if _, ok := getCachedNodeTemplateSet("node-a"); ok {
		t.Fatal("node removal should delete reverse locality state")
	}
}

func TestSyncAllFromDB_ExternalLoaderSyncsTemplates(t *testing.T) {
	origLoader := externalNodeLoader
	origCache := l.cache
	origImageCache := l.imageCache
	origTemplateNodeCache := l.templateNodeCache
	origSorted := l.sortedNodesByClusters
	defer func() {
		externalNodeLoader = origLoader
		l.cache = origCache
		l.imageCache = origImageCache
		l.templateNodeCache = origTemplateNodeCache
		l.sortedNodesByClusters = origSorted
	}()

	l.cache = cache.New(0, 0)
	l.imageCache = cache.New(0, 0)
	l.templateNodeCache = cache.New(0, 0)
	l.sortedNodesByClusters = map[string]node.NodeList{
		constants.DefaultInstanceTypeName: {},
	}

	externalNodeLoader = func(_ context.Context) ([]*node.Node, error) {
		return []*node.Node{
			{
				InsID:                  "node-1",
				IP:                     "10.0.0.1",
				Healthy:                true,
				MetaDataUpdateAt:       time.Now(),
				LocalTemplates:         []string{"tpl-1", "tpl-2"},
				LocalTemplatesReported: true,
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
	if templates, ok := getCachedNodeTemplateSet("node-1"); !ok {
		t.Fatal("expected node template membership cache")
	} else {
		if _, exists := templates["tpl-1"]; !exists {
			t.Fatal("tpl-1 missing from membership cache")
		}
		if _, exists := templates["tpl-2"]; !exists {
			t.Fatal("tpl-2 missing from membership cache")
		}
	}
}
